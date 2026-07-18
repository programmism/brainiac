package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestReindexHNSW verifies the online rebuild applies the requested build params
// to both HNSW indexes and is idempotent (#233).
func TestReindexHNSW(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed reindex test")
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

	reloptions := func(index string) string {
		t.Helper()
		var opts []string
		if err := pool.QueryRow(ctx,
			`SELECT coalesce(reloptions, '{}'::text[]) FROM pg_class WHERE relname = $1`, index).Scan(&opts); err != nil {
			t.Fatalf("read reloptions for %s: %v", index, err)
		}
		return strings.Join(opts, ",")
	}

	if err := ReindexHNSW(ctx, pool, 8, 32); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	for _, ix := range []string{"chunks_embedding_hot_idx", "nodes_summary_embedding_idx"} {
		opts := reloptions(ix)
		if !strings.Contains(opts, "m=8") || !strings.Contains(opts, "ef_construction=32") {
			t.Fatalf("%s reloptions = %q, want m=8 + ef_construction=32", ix, opts)
		}
	}

	// Idempotent: a second run (e.g. after a raised param) still succeeds and the
	// index survives with the new params.
	if err := ReindexHNSW(ctx, pool, 24, 100); err != nil {
		t.Fatalf("second reindex: %v", err)
	}
	if opts := reloptions("chunks_embedding_hot_idx"); !strings.Contains(opts, "m=24") || !strings.Contains(opts, "ef_construction=100") {
		t.Fatalf("re-run reloptions = %q, want m=24 + ef_construction=100", opts)
	}
	// The partial-index predicate must survive the rebuild (hot-only chunk index).
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'chunks_embedding_hot_idx'`).Scan(&def); err != nil {
		t.Fatalf("read indexdef: %v", err)
	}
	if !strings.Contains(def, "tier = 'hot'") {
		t.Fatalf("rebuilt index lost its WHERE predicate: %s", def)
	}
}
