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

func TestChunkTextWordBoundaryAndOverlap(t *testing.T) {
	// A long single paragraph of distinct words.
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&sb, "word%d ", i)
	}
	text := strings.TrimSpace(sb.String())
	const size = 100
	chunks := chunkText(text, size)

	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if n := len([]rune(c)); n > size {
			t.Errorf("chunk exceeds size: %d > %d", n, size)
		}
		// No chunk should end or start with a truncated "word<digits" fragment
		// that isn't a whole token; every token must match the word pattern.
		for _, tok := range strings.Fields(c) {
			if !strings.HasPrefix(tok, "word") {
				t.Errorf("mid-word split produced token %q", tok)
			}
		}
	}
	// Consecutive chunks overlap (share at least one word).
	inSecond := map[string]bool{}
	for _, w := range strings.Fields(chunks[1]) {
		inSecond[w] = true
	}
	overlap := false
	for _, w := range strings.Fields(chunks[0]) {
		if inSecond[w] {
			overlap = true
			break
		}
	}
	if !overlap {
		t.Errorf("expected consecutive chunks to overlap; got %q / %q", chunks[0], chunks[1])
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
