package store

import (
	"context"
	"os"
	"testing"
)

// TestMigrate runs the real migrations against the pgvector service container.
// It skips when DATABASE_URL is unset (e.g. local `go test` without a DB); CI
// always provides one.
func TestMigrate(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed migration test")
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
	// Idempotent: a second run must be a clean no-op.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate (second run): %v", err)
	}

	for _, table := range []string{"chunks", "nodes", "edges", "schema_migrations"} {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`,
			table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %q missing after migrate", table)
		}
	}

	// The current-tier partial indexes (#230) must exist: they keep the hot
	// working set small as historical rows accumulate.
	for _, index := range []string{
		"edges_from_current_idx", "edges_to_current_idx", "nodes_project_current_idx",
		"nodes_superseded_at_idx", "edges_superseded_at_idx",
	} {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`,
			index,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check index %s: %v", index, err)
		}
		if !exists {
			t.Errorf("index %q missing after migrate", index)
		}
	}
}
