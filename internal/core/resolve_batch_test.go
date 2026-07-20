package core

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmism/brainiac/internal/plugins"
)

// TestBatchResolvesCrossDocumentDuplicates drives cross-document entity resolution
// (#431): two documents in one batch mention the same entity under variant names
// ("OrderService" with alias "Order Service" in doc A, "Order Service" in doc B).
// Per-chunk resolution matches only exact canonical names, so it leaves two
// proposals; after the batch applies, resolveBatchDuplicates collapses them into one
// (survivor keeps the alias, the edge repoints), while an unrelated entity (Postgres)
// stays separate.
func TestBatchResolvesCrossDocumentDuplicates(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "TRUNCATE extraction_batches CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	c.extractReview = true // proposals awaiting review — the batch resolver's domain

	c.batchExtractor = &mockBatch{ended: true, results: map[string]plugins.Extraction{
		"d1": {Entities: []plugins.Entity{{Name: "OrderService", Aliases: []string{"Order Service"}}}},
		"d2": {
			Entities:  []plugins.Entity{{Name: "Order Service"}, {Name: "Postgres"}},
			Relations: []plugins.Relation{{From: "Order Service", Type: "uses", To: "Postgres", Why: "durable state"}},
		},
	}}

	if _, err := c.SubmitExtractionBatch(ctx, []BatchWorkItem{
		{CustomID: "d1", Text: "OrderService handles checkout.", SourceURI: "doc://a"},
		{CustomID: "d2", Text: "Order Service uses Postgres for durable state.", SourceURI: "doc://b"},
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := c.PollExtractionBatches(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// The two order-service mentions collapsed into ONE proposal; Postgres stays its
	// own. So 2 proposed nodes remain and 1 duplicate was retired (historical). The
	// batch applies its items in map order, so which name variant wins the survivor
	// is non-deterministic — the assertions below are written to not care.
	if n := countNodesByStatus(ctx, t, pool, "proposed"); n != 2 {
		t.Fatalf("proposed nodes = %d, want 2 (survivor + Postgres)", n)
	}
	if n := countNodesByStatus(ctx, t, pool, "historical"); n != 1 {
		t.Fatalf("retired duplicate nodes = %d, want 1", n)
	}

	// Exactly one proposed node normalizes to "orderservice" (the merged survivor).
	const normOrderService = `regexp_replace(lower(canonical_name),'[^a-z0-9]','','g')='orderservice'`
	var cnt int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE status='proposed' AND `+normOrderService).Scan(&cnt); err != nil {
		t.Fatalf("count survivors: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("order-service survivors = %d, want 1 (the two variants merged)", cnt)
	}

	// It holds BOTH name variants across its canonical name + aliases.
	var canonical string
	var aliases []string
	if err := pool.QueryRow(ctx, `SELECT canonical_name, aliases FROM nodes WHERE status='proposed' AND `+normOrderService).Scan(&canonical, &aliases); err != nil {
		t.Fatalf("load survivor: %v", err)
	}
	names := append([]string{canonical}, aliases...)
	if !containsStr(names, "OrderService") || !containsStr(names, "Order Service") {
		t.Fatalf("survivor name+aliases = %v, want both 'OrderService' and 'Order Service'", names)
	}

	// The 'uses' edge now hangs off the surviving order-service node, not a retired one.
	var fromNorm string
	if err := pool.QueryRow(ctx, `
		SELECT regexp_replace(lower(n.canonical_name),'[^a-z0-9]','','g')
		FROM edges e JOIN nodes n ON n.id = e.from_id
		WHERE e.type='uses' AND e.status='proposed'`).Scan(&fromNorm); err != nil {
		t.Fatalf("load uses edge: %v", err)
	}
	if fromNorm != "orderservice" {
		t.Fatalf("uses-edge from normalizes to %q, want 'orderservice' (survivor)", fromNorm)
	}
}

// TestResolveBatchDuplicatesKeepsDistinctEntities is the conservative guard: entities
// that are not the same identity (no shared normalized name/alias) are never merged.
func TestResolveBatchDuplicatesKeepsDistinctEntities(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()
	c.extractReview = true

	if _, _, err := c.applyExtraction(ctx, plugins.Extraction{Entities: []plugins.Entity{
		{Name: "OrderService"}, {Name: "PaymentService"}, {Name: "Kafka"},
	}}, "doc://x", nil, false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	merged, err := c.resolveBatchDuplicates(ctx, []string{""})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if merged != 0 {
		t.Fatalf("merged = %d, want 0 (all distinct)", merged)
	}
	if n := countNodesByStatus(ctx, t, pool, "proposed"); n != 3 {
		t.Fatalf("proposed nodes = %d, want 3 (none merged)", n)
	}
}

func countNodesByStatus(ctx context.Context, t *testing.T, pool *pgxpool.Pool, status string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE status = $1`, status).Scan(&n); err != nil {
		t.Fatalf("count nodes (%s): %v", status, err)
	}
	return n
}
