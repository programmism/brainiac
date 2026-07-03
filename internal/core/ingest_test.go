package core

import (
	"context"
	"fmt"
	"iter"
	"strings"
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

func TestIngestReembedsOnlyLocalRegion(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	var sb strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&sb, "Sentence %d: the quick brown fox jumps over the lazy dog by the river. ", i)
	}
	text := sb.String()
	const uri = "doc://big"

	s1, err := c.Ingest(ctx, sliceConn{docs: []plugins.RawDoc{{Text: text, SourceURI: uri}}}, IngestOptions{})
	if err != nil {
		t.Fatalf("ingest v1: %v", err)
	}
	stored := s1.Kept + s1.Queued // both are embedded + stored
	if stored < 8 {
		t.Fatalf("expected many chunks first time, stored=%d", stored)
	}

	// Edit near the very top and re-ingest.
	edited := text[:40] + "INSERTEDWORD " + text[40:]
	s2, err := c.Ingest(ctx, sliceConn{docs: []plugins.RawDoc{{Text: edited, SourceURI: uri}}}, IngestOptions{})
	if err != nil {
		t.Fatalf("ingest v2: %v", err)
	}
	// Content-defined boundaries self-heal: only a few chunks re-embed; the rest
	// are unchanged and skipped.
	if reembedded := s2.Kept + s2.Queued; reembedded > 3 {
		t.Errorf("early edit re-embedded %d chunks; expected <=3 (cascade!)", reembedded)
	}
	if s2.Skipped < stored-3 {
		t.Errorf("only %d chunks skipped; expected most of %d to be unchanged", s2.Skipped, stored)
	}
}

func TestSearchLensScopesByProject(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Same distinctive text ingested under two projects.
	const body = "PaymentGateway retries charges with idempotency keys during peak load."
	if _, err := c.IngestText(ctx, "doc://alpha", body, "alpha"); err != nil {
		t.Fatalf("ingest alpha: %v", err)
	}
	if _, err := c.IngestText(ctx, "doc://beta", body, "beta"); err != nil {
		t.Fatalf("ingest beta: %v", err)
	}
	// And a universal (global) doc.
	if _, err := c.IngestText(ctx, "doc://global", body, ""); err != nil {
		t.Fatalf("ingest global: %v", err)
	}

	// Lens for alpha sees alpha + global, not beta.
	hits, err := c.Search(ctx, body, 10, "alpha")
	if err != nil {
		t.Fatalf("search alpha: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hits {
		got[h.SourceURI] = true
	}
	if !got["doc://alpha"] || !got["doc://global"] {
		t.Fatalf("alpha lens should include alpha + global: %v", got)
	}
	if got["doc://beta"] {
		t.Fatalf("alpha lens must not include beta: %v", got)
	}

	// No project → spans all scopes.
	all, err := c.Search(ctx, body, 10, "")
	if err != nil {
		t.Fatalf("search all: %v", err)
	}
	allGot := map[string]bool{}
	for _, h := range all {
		allGot[h.SourceURI] = true
	}
	if !allGot["doc://alpha"] || !allGot["doc://beta"] || !allGot["doc://global"] {
		t.Fatalf("no-project search should span all: %v", allGot)
	}
}

func TestIngestText(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const uri = "notion://team-wiki"
	st, err := c.IngestText(ctx, uri, "OrderService streams events to Kafka for durability and audit.", "")
	if err != nil {
		t.Fatalf("ingest text: %v", err)
	}
	if st.Docs != 1 || st.Kept < 1 {
		t.Fatalf("ingest text stats: %+v", st)
	}

	// Editing the same source reconciles (old replaced, not accumulated).
	st2, err := c.IngestText(ctx, uri, "OrderService streams events to RabbitMQ now for throughput reasons.", "")
	if err != nil {
		t.Fatalf("re-ingest text: %v", err)
	}
	if st2.Deleted < 1 || st2.Kept < 1 {
		t.Fatalf("edit reconcile stats: %+v", st2)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks WHERE source_uri=$1", uri).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("chunks for %s = %d, want 1", uri, count)
	}

	if _, err := c.IngestText(ctx, "", "x", ""); err == nil {
		t.Error("empty source_uri should error")
	}
}

func TestIngestActualizesEditedDoc(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const uri = "doc://x"
	v1 := sliceConn{docs: []plugins.RawDoc{{Text: "OrderService writes 1200 orders to Postgres for durability.", SourceURI: uri}}}
	if _, err := c.Ingest(ctx, v1, IngestOptions{}); err != nil {
		t.Fatalf("ingest v1: %v", err)
	}

	// Edit the same source: old content must be replaced, not accumulated.
	v2 := sliceConn{docs: []plugins.RawDoc{{Text: "OrderService now writes 1200 orders to Kafka instead for throughput.", SourceURI: uri}}}
	s2, err := c.Ingest(ctx, v2, IngestOptions{})
	if err != nil {
		t.Fatalf("ingest v2: %v", err)
	}
	if s2.Deleted < 1 || s2.Kept < 1 {
		t.Fatalf("edit reconcile: deleted=%d kept=%d, want deleted>=1 kept>=1", s2.Deleted, s2.Kept)
	}

	// Exactly the new content remains for this source; the old chunk is gone.
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks WHERE source_uri=$1", uri).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("chunks for %s = %d, want 1 (stale replaced)", uri, count)
	}
	var text string
	if err := pool.QueryRow(ctx, "SELECT text FROM chunks WHERE source_uri=$1", uri).Scan(&text); err != nil {
		t.Fatalf("text: %v", err)
	}
	if !strings.Contains(text, "Kafka") {
		t.Fatalf("remaining chunk should be the edited (Kafka) text, got %q", text)
	}
}
