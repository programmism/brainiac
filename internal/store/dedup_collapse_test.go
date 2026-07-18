package store

import (
	"context"
	"os"
	"testing"

	"github.com/programmism/brainiac/internal/model"
)

// TestChunkDedupEnforcedByIndex: with the (content_hash, scope_key, trust) unique
// index (#393), identical content from two sources is stored once and InsertChunk
// links the second source's membership onto the existing chunk.
func TestChunkDedupEnforcedByIndex(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed dedup test")
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

	a := &model.Chunk{Text: "shared", Embedding: vec(1), SourceURI: "s://a", ContentHash: "hx"}
	if err := InsertChunk(ctx, pool, a); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	b := &model.Chunk{Text: "shared", Embedding: vec(1), SourceURI: "s://b", ContentHash: "hx"}
	if err := InsertChunk(ctx, pool, b); err != nil {
		t.Fatalf("insert b: %v", err)
	}
	if b.ID != a.ID {
		t.Fatalf("second source got a new chunk (%s) instead of linking to %s", b.ID, a.ID)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE content_hash = 'hx'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("dedup index failed: %d rows for content hx, want 1", n)
	}
	srcs, _ := ChunkSourceURIs(ctx, pool, a.ID)
	if len(srcs) != 2 || srcs[0] != "s://a" || srcs[1] != "s://b" {
		t.Fatalf("memberships = %v, want [s://a s://b]", srcs)
	}
}

// TestDedupCollapseMergesLegacyDuplicates exercises the destructive collapse of
// migration 0021: pre-existing duplicate-content rows (from before #389) are merged
// into one survivor, with every duplicate's membership repointed onto it — no
// source loses provenance.
func TestDedupCollapseMergesLegacyDuplicates(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed collapse test")
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

	// Drop the post-migration unique index so we can seed the legacy duplicate rows
	// it now forbids (this simulates a pre-#389 database). Recreate it afterward so
	// the shared test DB is left exactly as Migrate produced it (other tests, and
	// TestMigrate, rely on the index being present — Migrate is idempotent and won't
	// re-create it).
	if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS chunks_content_scope_trust_uniq`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	defer func() {
		_, _ = pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS chunks_content_scope_trust_uniq ON chunks (content_hash, scope_key, trust) WHERE content_hash IS NOT NULL`)
	}()
	// Two chunks with the same (content_hash, scope_key, trust) but different sources.
	var id1, id2 string
	if err := pool.QueryRow(ctx, `INSERT INTO chunks (text, source_uri, content_hash, scope_key, trust, tier, quality_score) VALUES ('dup', 'a://1', 'hd', '', 'trusted', 'hot', 1.0) RETURNING id`).Scan(&id1); err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO chunks (text, source_uri, content_hash, scope_key, trust, tier, quality_score) VALUES ('dup', 'a://2', 'hd', '', 'trusted', 'hot', 1.0) RETURNING id`).Scan(&id2); err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	if err := RecordChunkSource(ctx, pool, id1, "a://1"); err != nil {
		t.Fatalf("member 1: %v", err)
	}
	if err := RecordChunkSource(ctx, pool, id2, "a://2"); err != nil {
		t.Fatalf("member 2: %v", err)
	}

	// Run the collapse (mirrors steps 1 & 2 of migration 0021).
	if _, err := pool.Exec(ctx, `
		WITH survivors AS (
			SELECT content_hash, scope_key, trust, min(id::text)::uuid AS keep_id
			FROM chunks WHERE content_hash IS NOT NULL
			GROUP BY content_hash, scope_key, trust HAVING count(*) > 1)
		INSERT INTO chunk_sources (chunk_id, source_uri)
			SELECT s.keep_id, cs.source_uri FROM chunk_sources cs
			JOIN chunks c ON c.id = cs.chunk_id
			JOIN survivors s ON s.content_hash = c.content_hash AND s.scope_key = c.scope_key AND s.trust = c.trust
			WHERE cs.chunk_id <> s.keep_id ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("collapse merge: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		WITH survivors AS (
			SELECT content_hash, scope_key, trust, min(id::text)::uuid AS keep_id
			FROM chunks WHERE content_hash IS NOT NULL
			GROUP BY content_hash, scope_key, trust HAVING count(*) > 1)
		DELETE FROM chunks c USING survivors s
			WHERE c.content_hash = s.content_hash AND c.scope_key = s.scope_key AND c.trust = s.trust
			  AND c.id <> s.keep_id`); err != nil {
		t.Fatalf("collapse delete: %v", err)
	}

	// One survivor, and it carries both sources' provenance.
	var survivor string
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*), min(id::text) FROM chunks WHERE content_hash = 'hd'`).Scan(&n, &survivor); err != nil {
		t.Fatalf("post-collapse read: %v", err)
	}
	if n != 1 {
		t.Fatalf("collapse left %d rows for content hd, want 1", n)
	}
	srcs, _ := ChunkSourceURIs(ctx, pool, survivor)
	if len(srcs) != 2 || srcs[0] != "a://1" || srcs[1] != "a://2" {
		t.Fatalf("survivor memberships = %v, want [a://1 a://2] (no source lost)", srcs)
	}
}
