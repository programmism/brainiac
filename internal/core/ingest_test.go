package core

import (
	"context"
	"iter"
	"testing"

	"github.com/programmism/brainiac/internal/plugins"
)

type sliceConn struct{ docs []plugins.RawDoc }

func (c sliceConn) Fetch(context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		for _, d := range c.docs {
			if !yield(d, nil) {
				return
			}
		}
	}
}

func (sliceConn) Watch(context.Context) iter.Seq2[plugins.Change, error] {
	return func(func(plugins.Change, error) bool) {}
}

func TestIngestSelectsChunksAndIsIdempotent(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	conn := sliceConn{docs: []plugins.RawDoc{
		{Text: "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load.", SourceURI: "doc://a"},
		{Text: "hi", SourceURI: "doc://b"}, // too short → dropped by the selector
	}}

	s1, err := c.Ingest(ctx, conn, IngestOptions{})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if s1.Docs != 2 {
		t.Errorf("docs = %d, want 2", s1.Docs)
	}
	if s1.Kept < 1 {
		t.Errorf("kept = %d, want >= 1", s1.Kept)
	}
	if s1.Dropped < 1 {
		t.Errorf("dropped = %d, want >= 1 (the 'hi' doc)", s1.Dropped)
	}

	stored := s1.Kept + s1.Queued
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != stored {
		t.Errorf("stored chunks = %d, want %d", count, stored)
	}

	// Re-ingest: every stored chunk is unchanged → skipped, nothing new kept.
	s2, err := c.Ingest(ctx, conn, IngestOptions{})
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if s2.Kept != 0 || s2.Skipped < stored {
		t.Errorf("re-ingest: kept=%d skipped=%d, want kept=0 skipped>=%d", s2.Kept, s2.Skipped, stored)
	}
}
