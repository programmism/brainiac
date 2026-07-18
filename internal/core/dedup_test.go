package core

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

// TestGlobalContentDedup covers global content dedup across sources (#389):
// identical content ingested from a second source reuses the first source's chunk
// (no re-embed, no new row) and records the second source's membership, while a
// single-source re-ingest still skips and dedups nothing.
func TestGlobalContentDedup(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const shared = "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."

	// Source A stores the content.
	sA, err := c.Ingest(ctx, sliceConn{docs: []plugins.RawDoc{{Text: shared, SourceURI: "a://doc"}}}, IngestOptions{})
	if err != nil {
		t.Fatalf("ingest A: %v", err)
	}
	if sA.Kept == 0 {
		t.Fatalf("source A kept %d chunks, want >= 1", sA.Kept)
	}
	if sA.Deduped != 0 {
		t.Fatalf("source A deduped %d, want 0 (nothing to reuse yet)", sA.Deduped)
	}
	total0 := totalChunks(ctx, t, pool)
	ids := chunkIDsForSource(ctx, t, pool, "a://doc")

	// Source B ingests byte-identical content: it must be deduped — reused, not
	// re-embedded or re-stored.
	sB, err := c.Ingest(ctx, sliceConn{docs: []plugins.RawDoc{{Text: shared, SourceURI: "b://doc"}}}, IngestOptions{})
	if err != nil {
		t.Fatalf("ingest B: %v", err)
	}
	if sB.Deduped != sA.Kept {
		t.Fatalf("source B deduped %d, want %d (all of A's content reused)", sB.Deduped, sA.Kept)
	}
	if sB.Kept != 0 || sB.Queued != 0 {
		t.Fatalf("source B kept/queued %d/%d, want 0/0 (nothing new embedded)", sB.Kept, sB.Queued)
	}
	if n := totalChunks(ctx, t, pool); n != total0 {
		t.Fatalf("total chunks = %d after dedup, want %d (no new rows)", n, total0)
	}

	// The shared chunk now belongs to BOTH sources.
	for _, id := range ids {
		srcs, err := store.ChunkSourceURIs(ctx, pool, id)
		if err != nil {
			t.Fatalf("sources: %v", err)
		}
		if len(srcs) != 2 || srcs[0] != "a://doc" || srcs[1] != "b://doc" {
			t.Fatalf("chunk %s sources = %v, want [a://doc b://doc]", id, srcs)
		}
	}

	// Re-ingesting B is a clean skip (membership already vouches), deduping nothing.
	sB2, err := c.Ingest(ctx, sliceConn{docs: []plugins.RawDoc{{Text: shared, SourceURI: "b://doc"}}}, IngestOptions{})
	if err != nil {
		t.Fatalf("re-ingest B: %v", err)
	}
	if sB2.Deduped != 0 {
		t.Fatalf("re-ingest B deduped %d, want 0 (already a member)", sB2.Deduped)
	}
	if sB2.Skipped < sA.Kept {
		t.Fatalf("re-ingest B skipped %d, want >= %d (all content already present)", sB2.Skipped, sA.Kept)
	}
}

func totalChunks(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chunks`).Scan(&n); err != nil {
		t.Fatalf("count all chunks: %v", err)
	}
	return n
}
