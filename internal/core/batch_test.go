package core

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

type mockBatch struct {
	created int
	results map[string]plugins.Extraction
	ended   bool
}

func (m *mockBatch) CreateBatch(context.Context, []plugins.BatchItem) (string, error) {
	m.created++
	return "prov-batch-1", nil
}
func (m *mockBatch) FetchBatchResults(context.Context, string) (map[string]plugins.Extraction, bool, error) {
	return m.results, m.ended, nil
}

// TestBatchExtractionSubmitAndPoll drives the async batch lifecycle (#420): submit
// records the job + per-item context; poll (once ended) applies each result to the
// graph and marks the job applied; while still processing, nothing is applied.
func TestBatchExtractionSubmitAndPoll(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "TRUNCATE extraction_batches CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	mb := &mockBatch{ended: false, results: map[string]plugins.Extraction{
		"c1": {
			Entities:  []plugins.Entity{{Name: "OrderService", Type: "service"}, {Name: "Kafka", Type: "system"}},
			Relations: []plugins.Relation{{From: "OrderService", Type: "writes-to", To: "Kafka", Why: "durability"}},
		},
	}}
	c.batchExtractor = mb
	c.extractReview = false // write live so the assertions can read current rows

	jobID, err := c.SubmitExtractionBatch(ctx, []BatchWorkItem{
		{CustomID: "c1", Text: "OrderService writes to Kafka for durability", SourceURI: "doc://a", ForceReview: false},
	})
	if err != nil || jobID == "" {
		t.Fatalf("submit = (%q, %v)", jobID, err)
	}
	if mb.created != 1 {
		t.Fatalf("CreateBatch called %d times, want 1", mb.created)
	}

	// Job is submitted; its item context is stored.
	if jobs, _ := store.ExtractionBatchesByStatus(ctx, pool, store.BatchSubmitted); len(jobs) != 1 {
		t.Fatalf("submitted jobs = %d, want 1", len(jobs))
	}
	items, _ := store.BatchItemsForJob(ctx, pool, jobID)
	if it, ok := items["c1"]; !ok || it.SourceURI != "doc://a" {
		t.Fatalf("item c1 = %+v, want source doc://a", it)
	}

	// Still processing → poll applies nothing, job stays submitted.
	if n, err := c.PollExtractionBatches(ctx); err != nil || n != 0 {
		t.Fatalf("poll (processing) = (%d, %v), want 0", n, err)
	}
	if nodeCount(ctx, t, pool) != 0 {
		t.Fatal("nodes created before the batch ended")
	}

	// Batch ends → poll applies the results and marks the job applied.
	mb.ended = true
	n, err := c.PollExtractionBatches(ctx)
	if err != nil || n != 1 {
		t.Fatalf("poll (ended) = (%d, %v), want 1", n, err)
	}
	if nodeCount(ctx, t, pool) < 2 {
		t.Fatalf("want >=2 current nodes after apply, got %d", nodeCount(ctx, t, pool))
	}
	var edges int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM edges WHERE status = 'current'`).Scan(&edges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edges != 1 {
		t.Fatalf("want 1 current edge after apply, got %d", edges)
	}
	if jobs, _ := store.ExtractionBatchesByStatus(ctx, pool, store.BatchSubmitted); len(jobs) != 0 {
		t.Fatalf("still submitted after apply: %d", len(jobs))
	}
	if jobs, _ := store.ExtractionBatchesByStatus(ctx, pool, store.BatchApplied); len(jobs) != 1 {
		t.Fatalf("applied jobs = %d, want 1", len(jobs))
	}
}

// echoBatch captures the submitted custom_ids and returns a fixed extraction for
// each on fetch — so a test needn't know the chunker's content hashes up front.
type echoBatch struct {
	ids []string
	ext plugins.Extraction
}

func (m *echoBatch) CreateBatch(_ context.Context, items []plugins.BatchItem) (string, error) {
	for _, it := range items {
		m.ids = append(m.ids, it.CustomID)
	}
	return "prov-echo", nil
}
func (m *echoBatch) FetchBatchResults(context.Context, string) (map[string]plugins.Extraction, bool, error) {
	out := map[string]plugins.Extraction{}
	for _, id := range m.ids {
		out[id] = m.ext
	}
	return out, true, nil
}

// TestIngestSubmitsBatch drives the ingest→batch wiring (#430): with a batch
// extractor configured, ingest submits a batch instead of extracting synchronously
// (no graph written yet), and `poll-batches` then applies the results.
func TestIngestSubmitsBatch(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "TRUNCATE extraction_batches CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	c.batchExtractor = &echoBatch{ext: plugins.Extraction{
		Entities:  []plugins.Entity{{Name: "OrderService", Type: "service"}, {Name: "Kafka", Type: "system"}},
		Relations: []plugins.Relation{{From: "OrderService", Type: "writes-to", To: "Kafka", Why: "durability"}},
	}}
	c.extractReview = false

	const text = "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."
	stats, err := c.IngestTextWithTrust(ctx, "doc://batch", text, "", "trusted")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if stats.BatchesSubmitted != 1 {
		t.Fatalf("BatchesSubmitted = %d, want 1", stats.BatchesSubmitted)
	}
	if stats.ExtractedNodes != 0 {
		t.Fatalf("ExtractedNodes = %d, want 0 (deferred to poll)", stats.ExtractedNodes)
	}
	// Graph not written until the batch is applied.
	if nodeCount(ctx, t, pool) != 0 {
		t.Fatal("nodes created at ingest time; batch should defer them")
	}

	// Drain the batch → the extraction is applied.
	if n, err := c.PollExtractionBatches(ctx); err != nil || n != 1 {
		t.Fatalf("poll = (%d, %v), want 1", n, err)
	}
	if nodeCount(ctx, t, pool) < 2 {
		t.Fatalf("want >=2 nodes after poll, got %d", nodeCount(ctx, t, pool))
	}
}

func nodeCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE status = 'current'`).Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	return n
}
