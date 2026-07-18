package core

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

// errWatchConn lists its docs, then yields an error — to exercise the fail-safe.
type errWatchConn struct{ present []string }

func (errWatchConn) Fetch(context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(func(plugins.RawDoc, error) bool) {}
}
func (w errWatchConn) Watch(context.Context) iter.Seq2[plugins.Change, error] {
	return func(yield func(plugins.Change, error) bool) {
		for _, u := range w.present {
			if !yield(plugins.Change{SourceURI: u, Kind: plugins.ChangeUpserted}, nil) {
				return
			}
		}
		yield(plugins.Change{}, errors.New("listing error"))
	}
}

// TestSyncDeletions covers the poll-based deletion sync (#395): a document present
// in source_sync but absent from the connector's current listing is propagated as a
// deletion — only with the opt-in, only on a clean listing, and only when no other
// source still vouches for its chunks.
func TestSyncDeletions(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const ta = "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."
	const tb = "PaymentGateway retries failed charges with exponential backoff and reconciles nightly against the ledger."
	if _, err := c.IngestText(ctx, "markdown://a.md", ta, ""); err != nil {
		t.Fatalf("ingest a: %v", err)
	}
	if _, err := c.IngestText(ctx, "markdown://b.md", tb, ""); err != nil {
		t.Fatalf("ingest b: %v", err)
	}

	// Connector now lists only a.md as present (b.md was deleted at the source).
	lister := watchConn{changes: []plugins.Change{{SourceURI: "markdown://a.md", Kind: plugins.ChangeUpserted}}}

	// Opt-in off → nothing pruned.
	s, err := c.SyncDeletions(ctx, lister, "markdown://", IngestOptions{})
	if err != nil {
		t.Fatalf("sync (no opt-in): %v", err)
	}
	if s.DeletedDocs != 0 || countChunks(ctx, t, pool, "markdown://b.md") == 0 {
		t.Fatalf("prune happened without opt-in: DeletedDocs=%d", s.DeletedDocs)
	}

	// Fail-safe: a listing error disables pruning even with the opt-in.
	s, err = c.SyncDeletions(ctx, errWatchConn{present: []string{"markdown://a.md"}}, "markdown://", IngestOptions{PruneMissing: true})
	if err != nil {
		t.Fatalf("sync (error): %v", err)
	}
	if s.DeletedDocs != 0 || countChunks(ctx, t, pool, "markdown://b.md") == 0 {
		t.Fatalf("pruned despite a listing error: DeletedDocs=%d", s.DeletedDocs)
	}

	// Clean opt-in → b.md is propagated as a deletion; a.md untouched.
	s, err = c.SyncDeletions(ctx, lister, "markdown://", IngestOptions{PruneMissing: true})
	if err != nil {
		t.Fatalf("sync (opt-in): %v", err)
	}
	if s.DeletedDocs != 1 {
		t.Fatalf("DeletedDocs = %d, want 1 (b.md vanished)", s.DeletedDocs)
	}
	if countChunks(ctx, t, pool, "markdown://b.md") != 0 {
		t.Fatal("b.md not pruned")
	}
	if countChunks(ctx, t, pool, "markdown://a.md") == 0 {
		t.Fatal("a.md wrongly deleted")
	}
	if sourceSyncExists(ctx, t, pool, "markdown://b.md") {
		t.Fatal("b.md source_sync row survived")
	}
}

// TestSyncDeletionsKeepsMultiSource: a vanished doc's chunk survives when another
// source still vouches for it (membership-based, #387).
func TestSyncDeletionsKeepsMultiSource(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const ta = "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."
	if _, err := c.IngestText(ctx, "markdown://a.md", ta, ""); err != nil {
		t.Fatalf("ingest a: %v", err)
	}
	for _, id := range chunkIDsForSource(ctx, t, pool, "markdown://a.md") {
		if err := store.RecordChunkSource(ctx, pool, id, "notion://page-x"); err != nil {
			t.Fatalf("record second source: %v", err)
		}
	}

	// a.md vanishes from the listing.
	empty := watchConn{changes: nil}
	s, err := c.SyncDeletions(ctx, empty, "markdown://", IngestOptions{PruneMissing: true})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if s.DeletedDocs != 1 {
		t.Fatalf("DeletedDocs = %d, want 1 (a.md's markdown claim dropped)", s.DeletedDocs)
	}
	if countChunks(ctx, t, pool, "markdown://a.md") == 0 {
		t.Fatal("chunk deleted while notion://page-x still vouches for it")
	}
}
