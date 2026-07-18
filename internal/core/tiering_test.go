package core

import (
	"context"
	"testing"
	"time"
)

// TestSweepColdTier: the demotion policy archives hot chunks older than the
// window (and only those), and demoted chunks leave the hot search path (#231).
func TestSweepColdTier(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	if _, err := c.IngestText(ctx, "doc://old", "OrderService streams events to Kafka for durability and audit.", ""); err != nil {
		t.Fatalf("ingest old: %v", err)
	}
	if _, err := c.IngestText(ctx, "doc://new", "BillingService reconciles invoices against the ledger nightly.", ""); err != nil {
		t.Fatalf("ingest new: %v", err)
	}
	// Backdate the "old" document past the demotion window.
	if _, err := pool.Exec(ctx, `UPDATE chunks SET created_at = now() - interval '200 days' WHERE source_uri = 'doc://old'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	n, err := c.SweepColdTier(ctx, 180*24*time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected at least 1 chunk demoted, got %d", n)
	}

	tierOf := func(uri string) string {
		t.Helper()
		var tier string
		if err := pool.QueryRow(ctx, `SELECT tier FROM chunks WHERE source_uri = $1 LIMIT 1`, uri).Scan(&tier); err != nil {
			t.Fatalf("read tier for %s: %v", uri, err)
		}
		return tier
	}
	if got := tierOf("doc://old"); got != "cold" {
		t.Fatalf("old chunk tier = %q, want cold", got)
	}
	if got := tierOf("doc://new"); got != "hot" {
		t.Fatalf("recent chunk tier = %q, want hot (should not be demoted)", got)
	}

	// The demoted chunk leaves the default (hot-only) search path.
	hits, err := c.Search(ctx, "OrderService Kafka durability", 5, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, h := range hits {
		if h.SourceURI == "doc://old" {
			t.Fatalf("cold chunk still returned by hot search: %+v", h)
		}
	}

	// max_hot_age <= 0 is disabled (the CLI guards this, but the core rejects it).
	if _, err := c.SweepColdTier(ctx, 0); err == nil {
		t.Fatal("SweepColdTier(0) should error")
	}
}
