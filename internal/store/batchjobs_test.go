package store

import (
	"context"
	"os"
	"testing"
)

// TestExtractionBatchJobStore exercises the async-extraction job ledger (#383):
// insert a submitted batch, advance its status, and query the poller's work queue.
func TestExtractionBatchJobStore(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed batch-job test")
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
	if _, err := pool.Exec(ctx, "TRUNCATE extraction_batches"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	id, err := InsertExtractionBatch(ctx, pool, "msgbatch_abc")
	if err != nil || id == "" {
		t.Fatalf("insert: %v (id=%q)", err, id)
	}

	// Starts submitted, appears in the poller's queue.
	pending, err := ExtractionBatchesByStatus(ctx, pool, BatchSubmitted)
	if err != nil {
		t.Fatalf("list submitted: %v", err)
	}
	if len(pending) != 1 || pending[0].ProviderBatchID != "msgbatch_abc" {
		t.Fatalf("submitted = %+v, want one with provider id msgbatch_abc", pending)
	}

	// Advance to ended → leaves the submitted queue, joins the ended queue.
	if err := SetExtractionBatchStatus(ctx, pool, id, BatchEnded); err != nil {
		t.Fatalf("set ended: %v", err)
	}
	if s, _ := ExtractionBatchesByStatus(ctx, pool, BatchSubmitted); len(s) != 0 {
		t.Fatalf("still submitted after ending: %+v", s)
	}
	ended, err := ExtractionBatchesByStatus(ctx, pool, BatchEnded)
	if err != nil || len(ended) != 1 || ended[0].ID != id {
		t.Fatalf("ended = %+v, %v", ended, err)
	}
}
