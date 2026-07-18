package store

import (
	"context"
	"os"
	"testing"

	"github.com/programmism/brainiac/internal/model"
)

// TestNamespaceUsageCounter verifies the incremental counters (#229) stay exactly
// equal to count(*) across insert, delete, re-scope, and bulk delete, and that the
// migration's backfill reconstructs them from the live rows.
func TestNamespaceUsageCounter(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed namespace_usage test")
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
	// TRUNCATE does not fire the row/statement DELETE triggers, so reset the
	// counter table alongside the data.
	if _, err := pool.Exec(ctx, "TRUNCATE edges, nodes, chunks, namespace_usage"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	node := func(name, project string) {
		t.Helper()
		n := &model.Node{CanonicalName: name, Discriminators: map[string]string{"project": project}}
		if err := InsertNode(ctx, pool, n); err != nil {
			t.Fatalf("insert node %s: %v", name, err)
		}
	}
	chunk := func(uri, project string) {
		t.Helper()
		c := &model.Chunk{Text: uri, Embedding: vec(1), SourceURI: uri, QualityScore: 0.9,
			Discriminators: map[string]string{"project": project}}
		if err := InsertChunk(ctx, pool, c); err != nil {
			t.Fatalf("insert chunk %s: %v", uri, err)
		}
	}
	// assertUsage checks the counter equals the wanted values AND the exact count(*)
	// source of truth, so any trigger drift is caught.
	assertUsage := func(project string, wantNodes, wantChunks int) {
		t.Helper()
		n, ch, err := NamespaceUsage(ctx, pool, project)
		if err != nil {
			t.Fatalf("usage %q: %v", project, err)
		}
		wall := Namespaces([]string{project})
		cn, err := CountNodes(ctx, pool, wall)
		if err != nil {
			t.Fatalf("count nodes: %v", err)
		}
		cc, err := CountChunks(ctx, pool, wall)
		if err != nil {
			t.Fatalf("count chunks: %v", err)
		}
		if n != wantNodes || ch != wantChunks {
			t.Fatalf("project %q usage = (%d nodes, %d chunks), want (%d, %d)", project, n, ch, wantNodes, wantChunks)
		}
		if n != cn || ch != cc {
			t.Fatalf("project %q counter drifted from count(*): counter (%d,%d) vs count (%d,%d)", project, n, ch, cn, cc)
		}
	}

	// Inserts across two namespaces.
	node("A", "alpha")
	node("B", "alpha")
	node("C", "beta")
	chunk("u1", "alpha")
	chunk("u2", "beta")
	chunk("u3", "beta")
	assertUsage("alpha", 2, 1)
	assertUsage("beta", 1, 2)

	// Delete drops the count.
	if _, err := pool.Exec(ctx, `DELETE FROM nodes WHERE canonical_name = 'B'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertUsage("alpha", 1, 1)

	// Re-scope (disambiguate) moves a node between namespaces via an
	// UPDATE-of-discriminators, which the counter must follow.
	var cID string
	if err := pool.QueryRow(ctx, `SELECT id FROM nodes WHERE canonical_name = 'C'`).Scan(&cID); err != nil {
		t.Fatalf("find C: %v", err)
	}
	if err := UpdateNodeScope(ctx, pool, cID, map[string]string{"project": "alpha"}); err != nil {
		t.Fatalf("rescope: %v", err)
	}
	assertUsage("beta", 0, 2)
	assertUsage("alpha", 2, 1)

	// Bulk delete in a single statement is handled by the statement-level trigger.
	if _, err := pool.Exec(ctx, `DELETE FROM chunks WHERE project = 'beta'`); err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	assertUsage("beta", 0, 0)

	// Backfill reconstructs the counters from count(*) (the migration's seed step).
	if _, err := pool.Exec(ctx, `TRUNCATE namespace_usage`); err != nil {
		t.Fatalf("truncate usage: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO namespace_usage (project, nodes)
		SELECT project, count(*)::bigint FROM nodes GROUP BY project
		ON CONFLICT (project) DO UPDATE SET nodes = EXCLUDED.nodes`); err != nil {
		t.Fatalf("backfill nodes: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO namespace_usage (project, chunks)
		SELECT project, count(*)::bigint FROM chunks GROUP BY project
		ON CONFLICT (project) DO UPDATE SET chunks = EXCLUDED.chunks`); err != nil {
		t.Fatalf("backfill chunks: %v", err)
	}
	assertUsage("alpha", 2, 1)
	assertUsage("beta", 0, 0)
}
