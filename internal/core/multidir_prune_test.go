package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmism/brainiac/internal/plugins/markdown"
)

// TestMultiDirPrune drives deletion propagation across two docs dirs via one
// multi-root markdown connector (#391): a file removed from one dir is pruned on
// the next sweep, while the other dir's doc is untouched — and this now works with
// more than one dir (the old code disabled prune unless len(dirs)==1).
func TestMultiDirPrune(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	dirA, dirB := t.TempDir(), t.TempDir()
	const ta = "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."
	const tb = "PaymentGateway retries failed charges with exponential backoff and reconciles nightly against the ledger."
	writeFile(t, dirA, "a.md", ta)
	writeFile(t, dirB, "b.md", tb)

	sweep := func(prune bool) IngestStats {
		conn := markdown.NewMulti([]string{dirA, dirB})
		s, err := c.Ingest(ctx, conn, IngestOptions{PruneMissing: prune})
		if err != nil {
			t.Fatalf("ingest: %v", err)
		}
		return s
	}

	sweep(false)
	if n := distinctSources(ctx, t, pool); n != 2 {
		t.Fatalf("after ingest of two dirs: %d sources, want 2", n)
	}

	// Delete b.md and sweep with prune on — only b's doc should go.
	if err := os.Remove(filepath.Join(dirB, "b.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	s := sweep(true)
	if s.DeletedDocs != 1 {
		t.Fatalf("DeletedDocs = %d, want 1 (b.md vanished)", s.DeletedDocs)
	}
	if n := distinctSources(ctx, t, pool); n != 1 {
		t.Fatalf("after prune: %d sources, want 1 (a.md kept)", n)
	}
	// The surviving source is a.md's (dir A untouched).
	var survivingText string
	if err := pool.QueryRow(ctx, `SELECT text FROM chunks WHERE source_uri LIKE 'markdown://%' LIMIT 1`).Scan(&survivingText); err != nil {
		t.Fatalf("read surviving: %v", err)
	}
	if survivingText == "" {
		t.Fatal("dir A's document was wrongly deleted")
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func distinctSources(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT source_uri) FROM chunks WHERE source_uri LIKE 'markdown://%'`).Scan(&n); err != nil {
		t.Fatalf("count sources: %v", err)
	}
	return n
}
