package store

import (
	"context"
	"os"
	"testing"
)

// TestVacuumAnalyze checks that VacuumAnalyze runs (#385) — the point being that it
// uses the simple query protocol, since VACUUM cannot run inside pgx's default
// extended-protocol implicit transaction.
func TestVacuumAnalyze(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed vacuum test")
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
	if err := VacuumAnalyze(ctx, pool); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
}
