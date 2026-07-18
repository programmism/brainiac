package core

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

// TestPruneMissingPropagatesDeletions covers the opt-in deletion propagation
// (#247/#323): a document that vanished from a connector's sweep is removed only
// when PruneMissing is set; the default retains it; a fetch error disables it; and
// content another source still vouches for survives (membership-based, #387).
func TestPruneMissingPropagatesDeletions(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const ta = "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."
	const tb = "PaymentGateway retries failed charges with exponential backoff and reconciles nightly against the ledger."

	twoDocs := sliceConn{docs: []plugins.RawDoc{
		{Text: ta, SourceURI: "markdown://a.md"},
		{Text: tb, SourceURI: "markdown://b.md"},
	}}
	onlyA := sliceConn{docs: []plugins.RawDoc{{Text: ta, SourceURI: "markdown://a.md"}}}

	// Baseline: both docs ingested.
	if _, err := c.Ingest(ctx, twoDocs, IngestOptions{}); err != nil {
		t.Fatalf("ingest both: %v", err)
	}
	if n := countChunks(ctx, t, pool, "markdown://b.md"); n == 0 {
		t.Fatal("doc b stored no chunks")
	}

	// Default (PruneMissing off): b is absent from the sweep but retained (#107).
	s, err := c.Ingest(ctx, onlyA, IngestOptions{})
	if err != nil {
		t.Fatalf("ingest onlyA (retain): %v", err)
	}
	if s.DeletedDocs != 0 {
		t.Fatalf("DeletedDocs = %d with PruneMissing off, want 0", s.DeletedDocs)
	}
	if n := countChunks(ctx, t, pool, "markdown://b.md"); n == 0 {
		t.Fatal("doc b was deleted with PruneMissing off — retention default broken")
	}

	// Opt-in: b is now propagated as a deletion.
	s, err = c.Ingest(ctx, onlyA, IngestOptions{PruneMissing: true})
	if err != nil {
		t.Fatalf("ingest onlyA (prune): %v", err)
	}
	if s.DeletedDocs != 1 {
		t.Fatalf("DeletedDocs = %d, want 1 (doc b vanished)", s.DeletedDocs)
	}
	if n := countChunks(ctx, t, pool, "markdown://b.md"); n != 0 {
		t.Fatalf("doc b still has %d chunks after opt-in prune", n)
	}
	if sourceSyncExists(ctx, t, pool, "markdown://b.md") {
		t.Fatal("doc b source_sync row survived prune")
	}
	// Doc a is untouched.
	if n := countChunks(ctx, t, pool, "markdown://a.md"); n == 0 {
		t.Fatal("doc a wrongly deleted")
	}
}

// TestPruneMissingKeepsMultiSourceAndFailSafe covers two guards: a fetch error
// disables pruning, and a chunk another source vouches for survives.
func TestPruneMissingKeepsMultiSourceAndFailSafe(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Distinct content per doc so global dedup (#389) doesn't collapse them into one
	// chunk — this test is about membership across DIFFERENT sources, added below.
	const ta = "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."
	const tb = "PaymentGateway retries failed charges with exponential backoff and reconciles nightly against the ledger."

	both := sliceConn{docs: []plugins.RawDoc{
		{Text: ta, SourceURI: "markdown://a.md"},
		{Text: tb, SourceURI: "markdown://b.md"},
	}}
	if _, err := c.Ingest(ctx, both, IngestOptions{}); err != nil {
		t.Fatalf("ingest both: %v", err)
	}

	// A second source independently vouches for b's chunks.
	for _, id := range chunkIDsForSource(ctx, t, pool, "markdown://b.md") {
		if err := store.RecordChunkSource(ctx, pool, id, "notion://page-b"); err != nil {
			t.Fatalf("record second source: %v", err)
		}
	}

	// Fail-safe: a sweep with a fetch error must NOT prune even with the opt-in.
	// Doc a is fetched (index 0); index 1 yields a fetch error instead of doc b.
	errThenA := errConn{
		docs:  []plugins.RawDoc{{Text: ta, SourceURI: "markdown://a.md"}, {}},
		errAt: 1,
	}
	s, err := c.Ingest(ctx, errThenA, IngestOptions{PruneMissing: true})
	if err != nil {
		t.Fatalf("ingest errThenA: %v", err)
	}
	if s.FetchErrors == 0 {
		t.Fatal("test setup: expected a fetch error")
	}
	if s.DeletedDocs != 0 {
		t.Fatalf("DeletedDocs = %d after a fetch error, want 0 (fail-safe)", s.DeletedDocs)
	}
	if n := countChunks(ctx, t, pool, "markdown://b.md"); n == 0 {
		t.Fatal("doc b deleted despite a fetch error in the sweep")
	}

	// Clean opt-in sweep without b: b's markdown membership drops, but notion still
	// vouches, so the chunks survive (membership-based deletion, #387).
	onlyA := sliceConn{docs: []plugins.RawDoc{{Text: ta, SourceURI: "markdown://a.md"}}}
	s, err = c.Ingest(ctx, onlyA, IngestOptions{PruneMissing: true})
	if err != nil {
		t.Fatalf("ingest onlyA: %v", err)
	}
	if s.DeletedDocs != 1 {
		t.Fatalf("DeletedDocs = %d, want 1 (b's markdown membership dropped)", s.DeletedDocs)
	}
	if n := countChunks(ctx, t, pool, "markdown://b.md"); n == 0 {
		t.Fatal("multi-source chunk deleted while notion:// still vouches for it")
	}
}

func countChunks(ctx context.Context, t *testing.T, pool *pgxpool.Pool, uri string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chunks WHERE source_uri = $1`, uri).Scan(&n); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	return n
}

func sourceSyncExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, uri string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM source_sync WHERE source_uri = $1)`, uri).Scan(&exists); err != nil {
		t.Fatalf("source_sync exists: %v", err)
	}
	return exists
}
