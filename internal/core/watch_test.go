package core

import (
	"context"
	"iter"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

// watchConn is a connector whose Watch() replays a fixed list of changes (Fetch is
// a no-op) — to drive ApplyChanges in tests.
type watchConn struct{ changes []plugins.Change }

func (watchConn) Fetch(context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(func(plugins.RawDoc, error) bool) {}
}

func (w watchConn) Watch(context.Context) iter.Seq2[plugins.Change, error] {
	return func(yield func(plugins.Change, error) bool) {
		for _, ch := range w.changes {
			if !yield(ch, nil) {
				return
			}
		}
	}
}

// TestApplyChangesPropagatesDeletes covers the Watch()-driven deletion path (#323):
// a "deleted" change removes the document only when the opt-in is set, and only
// when no other source still vouches for its chunks (membership-based, #387).
func TestApplyChangesPropagatesDeletes(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const ta = "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."
	const tb = "PaymentGateway retries failed charges with exponential backoff and reconciles nightly against the ledger."

	ingest := func(uri, text string) {
		if _, err := c.IngestText(ctx, uri, text, ""); err != nil {
			t.Fatalf("ingest %s: %v", uri, err)
		}
	}
	ingest("doc://a", ta)
	ingest("doc://b", tb)

	delA := watchConn{changes: []plugins.Change{{SourceURI: "doc://a", Kind: plugins.ChangeDeleted}}}

	// Opt-in off: a delete change is ignored (retention default #107).
	s, err := c.ApplyChanges(ctx, delA, IngestOptions{})
	if err != nil {
		t.Fatalf("apply (no opt-in): %v", err)
	}
	if s.DeletedDocs != 0 {
		t.Fatalf("DeletedDocs = %d without opt-in, want 0", s.DeletedDocs)
	}
	if countChunks(ctx, t, pool, "doc://a") == 0 {
		t.Fatal("doc a deleted without opt-in")
	}

	// Opt-in on: doc a is removed.
	s, err = c.ApplyChanges(ctx, delA, IngestOptions{PruneMissing: true})
	if err != nil {
		t.Fatalf("apply (opt-in): %v", err)
	}
	if s.DeletedDocs != 1 {
		t.Fatalf("DeletedDocs = %d, want 1", s.DeletedDocs)
	}
	if n := countChunks(ctx, t, pool, "doc://a"); n != 0 {
		t.Fatalf("doc a still has %d chunks after delete", n)
	}
	if countChunks(ctx, t, pool, "doc://b") == 0 {
		t.Fatal("doc b wrongly deleted")
	}

	// Multi-source: a chunk another source still vouches for survives the delete.
	ingest("doc://c", ta) // c re-adds ta content (dedup may or may not apply; fresh here)
	for _, id := range chunkIDsForSource(ctx, t, pool, "doc://c") {
		if err := store.RecordChunkSource(ctx, pool, id, "notion://c2"); err != nil {
			t.Fatalf("record second source: %v", err)
		}
	}
	delC := watchConn{changes: []plugins.Change{{SourceURI: "doc://c", Kind: plugins.ChangeDeleted}}}
	s, err = c.ApplyChanges(ctx, delC, IngestOptions{PruneMissing: true})
	if err != nil {
		t.Fatalf("apply delC: %v", err)
	}
	if s.DeletedDocs != 1 {
		t.Fatalf("DeletedDocs = %d for delC, want 1", s.DeletedDocs)
	}
	if countChunks(ctx, t, pool, "doc://c") == 0 {
		t.Fatal("multi-source chunk deleted while notion://c2 still vouches for it")
	}
}
