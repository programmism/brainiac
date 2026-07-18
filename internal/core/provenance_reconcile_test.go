package core

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmism/brainiac/internal/store"
)

// TestReconcileKeepsMultiSourceChunk drives the real ingest reconcile (#387):
// when a source re-ingests without content that another source still vouches for,
// the chunk survives — but a single-source chunk is still deleted, exactly as
// before. Proves the membership-based delete replaced the per-source delete
// without changing single-source behavior.
func TestReconcileKeepsMultiSourceChunk(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const t1 = "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."
	const t2 = "PaymentGateway retries failed charges with exponential backoff and reconciles nightly against the ledger."

	// Source A ingests t1.
	if _, err := c.IngestText(ctx, "doc://a", t1, ""); err != nil {
		t.Fatalf("ingest A(t1): %v", err)
	}
	ids := chunkIDsForSource(ctx, t, pool, "doc://a")
	if len(ids) == 0 {
		t.Fatal("no chunks stored for doc://a")
	}

	// Source B independently vouches for the same content (what global dedup will
	// do automatically; here we record membership directly).
	for _, id := range ids {
		if err := store.RecordChunkSource(ctx, pool, id, "doc://b"); err != nil {
			t.Fatalf("record B membership: %v", err)
		}
	}

	// Source A re-ingests with entirely different content: A's claim on t1 drops,
	// but B still vouches — so the t1 chunks must survive the reconcile.
	if _, err := c.IngestText(ctx, "doc://a", t2, ""); err != nil {
		t.Fatalf("re-ingest A(t2): %v", err)
	}
	for _, id := range ids {
		if ok := chunkExistsCore(ctx, t, pool, id); !ok {
			t.Fatalf("chunk %s deleted while source B still vouches for it", id)
		}
		srcs, err := store.ChunkSourceURIs(ctx, pool, id)
		if err != nil {
			t.Fatalf("sources: %v", err)
		}
		if len(srcs) != 1 || srcs[0] != "doc://b" {
			t.Fatalf("chunk %s sources = %v, want [doc://b] (A's claim dropped)", id, srcs)
		}
	}

	// Single-source control: source C ingests t1's content, then re-ingests
	// different content. With no other source vouching, the chunk is pruned —
	// the old per-source-delete behavior, preserved.
	if _, err := c.IngestText(ctx, "doc://c", t1, ""); err != nil {
		t.Fatalf("ingest C(t1): %v", err)
	}
	cIDs := chunkIDsForSource(ctx, t, pool, "doc://c")
	if len(cIDs) == 0 {
		t.Fatal("no chunks stored for doc://c")
	}
	if _, err := c.IngestText(ctx, "doc://c", t2, ""); err != nil {
		t.Fatalf("re-ingest C(t2): %v", err)
	}
	for _, id := range cIDs {
		if chunkExistsCore(ctx, t, pool, id) {
			t.Fatalf("single-source chunk %s survived re-ingest of different content", id)
		}
	}
}

func chunkIDsForSource(ctx context.Context, t *testing.T, pool *pgxpool.Pool, uri string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id FROM chunks WHERE source_uri = $1 ORDER BY id`, uri)
	if err != nil {
		t.Fatalf("query chunk ids: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return ids
}

func chunkExistsCore(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chunks WHERE id = $1)`, id).Scan(&exists); err != nil {
		t.Fatalf("exists check: %v", err)
	}
	return exists
}
