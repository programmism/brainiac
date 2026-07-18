package core

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/plugins"
)

// TestIngestTrustTagging: connector-style ingest is untrusted, IngestText is
// trusted, and the tag is surfaced in retrieval results (#273).
func TestIngestTrustTagging(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Trusted is explicit opt-in; the default (IngestText, bulk Ingest) is untrusted.
	var stats IngestStats
	if err := c.ingestDoc(ctx, plugins.RawDoc{SourceURI: "doc://trusted",
		Text: "BillingService reconciles invoices against the ledger every night for audit."},
		IngestOptions{Trust: model.TrustTrusted}, &stats); err != nil {
		t.Fatalf("ingest trusted: %v", err)
	}
	// Default path (no Trust set) → untrusted (fail-closed).
	if _, err := c.IngestText(ctx, "doc://untrusted", "OrderService streams events to Kafka for durability and later audit.", ""); err != nil {
		t.Fatalf("ingest untrusted: %v", err)
	}

	trustOf := func(uri string) string {
		t.Helper()
		var trust string
		if err := pool.QueryRow(ctx, `SELECT trust FROM chunks WHERE source_uri = $1 LIMIT 1`, uri).Scan(&trust); err != nil {
			t.Fatalf("read trust for %s: %v", uri, err)
		}
		return trust
	}
	if got := trustOf("doc://trusted"); got != model.TrustTrusted {
		t.Fatalf("IngestText chunk trust = %q, want trusted", got)
	}
	if got := trustOf("doc://untrusted"); got != model.TrustUntrusted {
		t.Fatalf("bulk ingest chunk trust = %q, want untrusted", got)
	}

	// The tag rides through retrieval so a client can weigh recalled text.
	hits, err := c.Search(ctx, "OrderService Kafka durability", 5, "", false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var seen bool
	for _, h := range hits {
		if h.SourceURI == "doc://untrusted" {
			seen = true
			if h.Trust != model.TrustUntrusted {
				t.Fatalf("search hit trust = %q, want untrusted", h.Trust)
			}
		}
	}
	if !seen {
		t.Fatalf("untrusted chunk not returned by search: %+v", hits)
	}
}

// TestUntrustedForcesExtractionReview: with review OFF, an untrusted doc still
// writes extracted nodes as 'proposed' (never live), while a trusted doc honors
// the disabled-review setting and writes them 'current' (#273).
func TestUntrustedForcesExtractionReview(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	c.extractor = fakeExtractor{sampleExtraction()}
	c.extractReview = false // review disabled — untrusted must override this

	// Untrusted in project "u", trusted in project "t"; the extractor emits the same
	// entities, so scoping keeps them distinct for the assertion.
	var stats IngestStats
	if err := c.ingestDoc(ctx, plugins.RawDoc{SourceURI: "doc://u", Text: "OrderService writes to OrdersDB nightly for durability."},
		IngestOptions{Project: "u"}, &stats); err != nil { // no Trust → untrusted
		t.Fatalf("ingest untrusted: %v", err)
	}
	if err := c.ingestDoc(ctx, plugins.RawDoc{SourceURI: "doc://t", Text: "OrderService writes to OrdersDB nightly for durability."},
		IngestOptions{Project: "t", Trust: model.TrustTrusted}, &stats); err != nil {
		t.Fatalf("ingest trusted: %v", err)
	}

	statusOf := func(project string) string {
		t.Helper()
		var status string
		if err := pool.QueryRow(ctx,
			`SELECT status FROM nodes WHERE canonical_name = 'OrderService' AND project = $1`, project).Scan(&status); err != nil {
			t.Fatalf("read node status for %s: %v", project, err)
		}
		return status
	}
	if got := statusOf("u"); got != string(model.StatusProposed) {
		t.Fatalf("untrusted extraction status = %q, want proposed (review forced)", got)
	}
	if got := statusOf("t"); got != string(model.StatusCurrent) {
		t.Fatalf("trusted extraction status = %q, want current (review off honored)", got)
	}

	// The extracted edge carries the source's trust (#367), so it stays flagged
	// even after approval.
	edgeTrust := func(project string) string {
		t.Helper()
		var trust string
		if err := pool.QueryRow(ctx,
			`SELECT e.trust FROM edges e JOIN nodes n ON n.id = e.from_id WHERE n.project = $1 LIMIT 1`, project).Scan(&trust); err != nil {
			t.Fatalf("read edge trust for %s: %v", project, err)
		}
		return trust
	}
	if got := edgeTrust("u"); got != model.TrustUntrusted {
		t.Fatalf("edge from untrusted source trust = %q, want untrusted", got)
	}
	if got := edgeTrust("t"); got != model.TrustTrusted {
		t.Fatalf("edge from trusted source trust = %q, want trusted", got)
	}
}
