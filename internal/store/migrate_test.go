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
}
