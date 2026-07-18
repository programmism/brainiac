package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"
	"time"

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

// errConn yields a fetch error at index errAt, then keeps yielding — simulating a
// paginated connector that hits one bad page mid-backfill.
type errConn struct {
	docs  []plugins.RawDoc
	errAt int
}

func (c errConn) Fetch(context.Context) iter.Seq2[plugins.RawDoc, error] {
	return func(yield func(plugins.RawDoc, error) bool) {
		for i, d := range c.docs {
			if i == c.errAt {
				if !yield(plugins.RawDoc{}, errors.New("simulated fetch error")) {
					return
				}
				continue
			}
			if !yield(d, nil) {
				return
			}
		}
	}
}

func (errConn) Watch(context.Context) iter.Seq2[plugins.Change, error] {
	return func(func(plugins.Change, error) bool) {}
}

// A single fetch error is counted and skipped, not fatal — the good docs on
// either side still import (#241).
func TestIngestNonFatalFetchError(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	conn := errConn{
		errAt: 1,
		docs: []plugins.RawDoc{
			{Text: "OrderService writes 1200 orders to Postgres for durability during peak load.", SourceURI: "doc://a"},
			{}, // index 1 → yields the fetch error
			{Text: "PaymentGateway retries charges with idempotency keys during peak load.", SourceURI: "doc://b"},
		},
	}
	s, err := c.Ingest(ctx, conn, IngestOptions{})
	if err != nil {
		t.Fatalf("a mid-stream fetch error must not fail the whole ingest: %v", err)
	}
	if s.FetchErrors != 1 {
		t.Errorf("FetchErrors = %d, want 1", s.FetchErrors)
	}
	if s.Docs != 2 {
		t.Errorf("Docs = %d, want 2 (both good docs past the error)", s.Docs)
	}
	if s.Kept < 2 {
		t.Errorf("Kept = %d, want >= 2 (both good docs stored)", s.Kept)
	}
}

func TestIncrementalMtimeSkip(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const uri = "doc://inc"
	body := "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load."
	t1 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	docAt := func(tm time.Time) sliceConn {
		return sliceConn{docs: []plugins.RawDoc{{Text: body, SourceURI: uri, ModifiedAt: &tm}}}
	}

	// First incremental ingest stores chunks and records the sync point.
	s1, err := c.Ingest(ctx, docAt(t1), IngestOptions{Incremental: true})
	if err != nil {
		t.Fatalf("ingest v1: %v", err)
	}
	stored := s1.Kept + s1.Queued
	if stored < 1 || s1.SkippedDocs != 0 {
		t.Fatalf("v1: stored=%d skippedDocs=%d, want stored>=1 skippedDocs=0", stored, s1.SkippedDocs)
	}

	// Re-ingest with the SAME mtime under Incremental → whole doc skipped before
	// chunking (no chunks even counted).
	s2, err := c.Ingest(ctx, docAt(t1), IngestOptions{Incremental: true})
	if err != nil {
		t.Fatalf("ingest v2: %v", err)
	}
	if s2.SkippedDocs != 1 || s2.Chunks != 0 || s2.Kept != 0 {
		t.Fatalf("v2: %+v — want SkippedDocs=1, Chunks=0, Kept=0", s2)
	}

	// Without Incremental, the same mtime does NOT skip — the doc is reconciled
	// (content unchanged → chunks skipped by hash, but the doc is processed).
	s3, err := c.Ingest(ctx, docAt(t1), IngestOptions{})
	if err != nil {
		t.Fatalf("ingest v3: %v", err)
	}
	if s3.SkippedDocs != 0 || s3.Chunks == 0 {
		t.Fatalf("v3: %+v — want SkippedDocs=0 and the doc processed (Chunks>0)", s3)
	}

	// A newer mtime under Incremental processes the doc again (mtime advanced),
	// though its content is unchanged so chunks are skipped by hash.
	s4, err := c.Ingest(ctx, docAt(t1.Add(time.Hour)), IngestOptions{Incremental: true})
	if err != nil {
		t.Fatalf("ingest v4: %v", err)
	}
	if s4.SkippedDocs != 0 || s4.Skipped < stored {
		t.Fatalf("v4: %+v — want SkippedDocs=0 and unchanged chunks skipped by hash", s4)
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
	hits, err := c.Search(ctx, body, 10, "alpha", false)
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
	all, err := c.Search(ctx, body, 10, "", false)
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

func TestRecallScopeProvenance(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const body = "PaymentGateway retries charges with idempotency keys during peak load."
	if _, err := c.IngestText(ctx, "doc://alpha", body, "alpha"); err != nil {
		t.Fatalf("ingest alpha: %v", err)
	}
	if _, err := c.IngestText(ctx, "doc://global", body, ""); err != nil {
		t.Fatalf("ingest global: %v", err)
	}

	// Search under alpha: hits carry a scope label — alpha's own vs the global doc.
	hits, err := c.Search(ctx, body, 10, "alpha", false)
	if err != nil {
		t.Fatalf("search alpha: %v", err)
	}
	scopes := map[string]string{}
	for _, h := range hits {
		scopes[h.SourceURI] = h.Scope
	}
	if scopes["doc://alpha"] != "project:alpha" {
		t.Errorf("alpha hit scope = %q, want project:alpha", scopes["doc://alpha"])
	}
	if scopes["doc://global"] != "global" {
		t.Errorf("global hit scope = %q, want global", scopes["doc://global"])
	}

	// Recall against a project with no content: everything returned is global, so
	// it must be flagged as a fallback (#143).
	empty, err := c.Recall(ctx, body, "neznaika")
	if err != nil {
		t.Fatalf("recall neznaika: %v", err)
	}
	if empty.Scope != "project:neznaika" {
		t.Errorf("recall scope = %q, want project:neznaika", empty.Scope)
	}
	if len(empty.Chunks) == 0 {
		t.Fatal("expected global fallback chunks, got none")
	}
	if !empty.ScopeFallback {
		t.Error("recall against empty project returning only global should set ScopeFallback")
	}

	// Recall against a project that has content: not a fallback.
	got, err := c.Recall(ctx, body, "alpha")
	if err != nil {
		t.Fatalf("recall alpha: %v", err)
	}
	if got.ScopeFallback {
		t.Error("recall with in-project results must not flag fallback")
	}
}

func TestIngestDryRun(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	conn := sliceConn{docs: []plugins.RawDoc{
		{Text: "OrderService writes 1200 orders to Postgres and Kafka every minute for durability during peak load.", SourceURI: "doc://dry"},
	}}

	dry, err := c.Ingest(ctx, conn, IngestOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Kept < 1 {
		t.Fatalf("dry run should report kept>=1: %+v", dry)
	}

	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks WHERE source_uri='doc://dry'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("dry run must not write, but %d chunks were stored", count)
	}

	// A real ingest of the same input matches what the dry run predicted.
	live, err := c.Ingest(ctx, conn, IngestOptions{})
	if err != nil {
		t.Fatalf("live ingest: %v", err)
	}
	if live.Kept != dry.Kept || live.Queued != dry.Queued || live.Dropped != dry.Dropped {
		t.Fatalf("dry-run prediction != real: dry=%+v live=%+v", dry, live)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM chunks WHERE source_uri='doc://dry'").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != live.Kept+live.Queued {
		t.Fatalf("stored=%d, want %d", count, live.Kept+live.Queued)
	}
}

func TestIngestProgressReported(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	var sb strings.Builder
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&sb, "Sentence %d: the quick brown fox jumps over the lazy dog by the river. ", i)
	}

	var calls, lastEmbedded, toEmbed int
	monotonic, prev := true, -1
	opts := IngestOptions{OnProgress: func(p IngestProgress) {
		calls++
		if p.Embedded < prev {
			monotonic = false
		}
		prev = p.Embedded
		lastEmbedded, toEmbed = p.Embedded, p.ToEmbed
	}}
	if _, err := c.Ingest(ctx, sliceConn{docs: []plugins.RawDoc{{Text: sb.String(), SourceURI: "doc://prog"}}}, opts); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected multiple progress callbacks, got %d", calls)
	}
	if toEmbed == 0 || lastEmbedded != toEmbed {
		t.Fatalf("final progress should reach ToEmbed: embedded=%d toEmbed=%d", lastEmbedded, toEmbed)
	}
	if !monotonic {
		t.Fatal("Embedded should be non-decreasing across callbacks")
	}
}

// Ingest stamps each chunk with passage-level provenance — a char offset and the
// nearest preceding Markdown heading (#243).
func TestIngestStampsPassageProvenance(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	var sb strings.Builder
	sb.WriteString("# Order pipeline\n\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&sb, "OrderService writes 1200 orders to Kafka for durability during peak load, point %d. ", i)
	}
	if _, err := c.IngestText(ctx, "doc://prov", sb.String(), ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	rows, err := pool.Query(ctx, "SELECT source_locator FROM chunks WHERE source_uri = 'doc://prov'")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	n, sawHeading := 0, false
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var loc map[string]any
		if err := json.Unmarshal(raw, &loc); err != nil {
			t.Fatalf("locator json: %v", err)
		}
		if _, ok := loc["char_offset"]; !ok {
			t.Errorf("chunk missing char_offset: %v", loc)
		}
		if loc["heading"] == "Order pipeline" {
			sawHeading = true
		}
		n++
	}
	if n == 0 {
		t.Fatal("no chunks stored")
	}
	if !sawHeading {
		t.Error("no chunk carried the 'Order pipeline' heading anchor")
	}
}
