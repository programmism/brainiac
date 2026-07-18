package store

import (
	"context"
	"os"
	"testing"

	"github.com/programmism/brainiac/internal/model"
)

// TestChunkSourcesMembership exercises the multi-source provenance keystone
// (#244): a chunk vouched for by two sources survives one source dropping it, and
// is pruned only when its last source is gone.
func TestChunkSourcesMembership(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed provenance test")
	}
	ctx := context.Background()

	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE chunk_sources, chunks CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// A chunk ingested from source A. InsertChunk records its membership.
	c := &model.Chunk{Text: "shared knowledge", Embedding: vec(3), SourceURI: "a://doc", ContentHash: "h1"}
	if err := InsertChunk(ctx, pool, c); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	if c.ID == "" {
		t.Fatal("insert did not set chunk id")
	}

	// The same content is also vouched for by source B.
	if err := RecordChunkSource(ctx, pool, c.ID, "b://doc"); err != nil {
		t.Fatalf("record second source: %v", err)
	}
	if got, _ := ChunkSourceURIs(ctx, pool, c.ID); len(got) != 2 {
		t.Fatalf("want 2 sources, got %v", got)
	}

	// Source A drops the chunk (its content is gone from A). Membership for A is
	// removed, but B still vouches for it — so an orphan prune must NOT delete it.
	if _, err := DropChunkSourceMembershipNotIn(ctx, pool, "a://doc", nil); err != nil {
		t.Fatalf("drop A membership: %v", err)
	}
	if got, _ := ChunkSourceURIs(ctx, pool, c.ID); len(got) != 1 || got[0] != "b://doc" {
		t.Fatalf("want only b://doc after A drop, got %v", got)
	}
	if n, err := PruneOrphanChunks(ctx, pool); err != nil || n != 0 {
		t.Fatalf("prune after A drop = (%d, %v), want (0, nil) — B still vouches", n, err)
	}
	if ok, _ := chunkExists(ctx, pool, c.ID); !ok {
		t.Fatal("chunk deleted while source B still vouches for it")
	}

	// Now B drops it too. With no sources left, the prune removes the chunk.
	if _, err := DropChunkSourceMembershipNotIn(ctx, pool, "b://doc", nil); err != nil {
		t.Fatalf("drop B membership: %v", err)
	}
	if n, err := PruneOrphanChunks(ctx, pool); err != nil || n != 1 {
		t.Fatalf("prune after last source = (%d, %v), want (1, nil)", n, err)
	}
	if ok, _ := chunkExists(ctx, pool, c.ID); ok {
		t.Fatal("orphan chunk survived prune after its last source was dropped")
	}
}

func chunkExists(ctx context.Context, db DBTX, id string) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chunks WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}
