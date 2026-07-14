package core

import (
	"context"
	"hash/fnv"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/plugins/density"
	"github.com/programmism/brainiac/internal/store"
)

// hashEmbedder maps identical text to an identical one-hot vector, so equal
// summaries collide (distance 0) and different ones are orthogonal (distance 1).
type hashEmbedder struct{}

func (hashEmbedder) Dims() int { return 768 }
func (hashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(text))
	v := make([]float32, 768)
	v[h.Sum32()%768] = 1
	return v, nil
}

// EmbedBatch makes hashEmbedder a plugins.BatchEmbedder so ingest exercises the
// batch path (#140); it maps Embed over the inputs, preserving order.
func (e hashEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := e.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func newTestCore(t *testing.T) (*Core, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed core test")
	}
	ctx := context.Background()
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE edges, nodes, chunks"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return New(pool, hashEmbedder{}, density.New()), pool
}

func TestRememberCreateIdempotentAndDedup(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Create.
	r1, err := c.Remember(ctx, RememberInput{CanonicalName: "OrderService", Type: "service", Summary: "handles orders"})
	if err != nil {
		t.Fatalf("remember 1: %v", err)
	}
	if !r1.Created || len(r1.Duplicates) != 0 {
		t.Fatalf("first remember: created=%v dups=%v", r1.Created, r1.Duplicates)
	}

	// Idempotent exact-name re-remember merges aliases.
	r2, err := c.Remember(ctx, RememberInput{CanonicalName: "OrderService", Aliases: []string{"OrderSvc"}})
	if err != nil {
		t.Fatalf("remember 2: %v", err)
	}
	if r2.Created {
		t.Fatal("exact-name re-remember should not create")
	}
	if len(r2.Node.Aliases) != 1 || r2.Node.Aliases[0] != "OrderSvc" {
		t.Fatalf("aliases not merged: %+v", r2.Node.Aliases)
	}

	// Normalized-name duplicate flag ("Order Service" ~ "OrderService").
	r3, err := c.Remember(ctx, RememberInput{CanonicalName: "Order Service"})
	if err != nil {
		t.Fatalf("remember 3: %v", err)
	}
	if !r3.Created || !hasDup(r3.Duplicates, "OrderService", "normalized-name") {
		t.Fatalf("expected normalized-name dup: %+v", r3.Duplicates)
	}

	// Semantic duplicate flag (same summary embedding as OrderService).
	r4, err := c.Remember(ctx, RememberInput{CanonicalName: "Billing", Summary: "handles orders"})
	if err != nil {
		t.Fatalf("remember 4: %v", err)
	}
	if !hasDup(r4.Duplicates, "OrderService", "semantic") {
		t.Fatalf("expected semantic dup: %+v", r4.Duplicates)
	}
}

func TestRememberPersistsSummaryAndGetNode(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	r, err := c.Remember(ctx, RememberInput{
		CanonicalName: "Ada Lovelace", Type: "person", Aliases: []string{"Ace"},
		Summary: "English mathematician; wrote the first algorithm.",
	})
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	if r.Node.Summary != "English mathematician; wrote the first algorithm." {
		t.Fatalf("summary not on returned node: %q", r.Node.Summary)
	}

	// The summary text round-trips through a direct read (not just the embedding).
	stored, err := store.GetNodeByID(ctx, pool, r.Node.ID)
	if err != nil || stored == nil {
		t.Fatalf("get by id: %v (node=%v)", err, stored)
	}
	if stored.Summary != r.Node.Summary {
		t.Fatalf("stored summary %q != %q", stored.Summary, r.Node.Summary)
	}

	// GetNode by name surfaces the full record — aliases + summary — for citation.
	det, err := c.GetNode(ctx, "", "Ada Lovelace", "")
	if err != nil || det == nil {
		t.Fatalf("get node: %v (det=%v)", err, det)
	}
	if det.Node.Summary != r.Node.Summary || len(det.Node.Aliases) != 1 || det.Node.Aliases[0] != "Ace" {
		t.Fatalf("get node record: summary=%q aliases=%v", det.Node.Summary, det.Node.Aliases)
	}

	// Re-remembering with a new description backfills/updates the summary in place.
	r2, err := c.Remember(ctx, RememberInput{CanonicalName: "Ada Lovelace", Summary: "Countess of Lovelace."})
	if err != nil {
		t.Fatalf("re-remember: %v", err)
	}
	if r2.Created {
		t.Fatal("exact-name re-remember should not create")
	}
	after, err := store.GetNodeByID(ctx, pool, r.Node.ID)
	if err != nil || after == nil {
		t.Fatalf("get by id after update: %v", err)
	}
	if after.Summary != "Countess of Lovelace." {
		t.Fatalf("summary not updated on re-remember: %q", after.Summary)
	}
}

func TestRememberScopedIdentity(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Same name, different projects → two distinct nodes, neither flagged as a
	// duplicate of the other (identity = name + discriminators, #117).
	a, err := c.Remember(ctx, RememberInput{CanonicalName: "Config", Discriminators: map[string]string{"project": "goroutly"}})
	if err != nil {
		t.Fatalf("remember A: %v", err)
	}
	b, err := c.Remember(ctx, RememberInput{CanonicalName: "Config", Discriminators: map[string]string{"project": "brainiac"}})
	if err != nil {
		t.Fatalf("remember B: %v", err)
	}
	if !a.Created || !b.Created {
		t.Fatalf("both scoped Configs should be created: a=%v b=%v", a.Created, b.Created)
	}
	if a.Node.ID == b.Node.ID {
		t.Fatal("scoped Configs collapsed into one node")
	}
	if len(b.Duplicates) != 0 {
		t.Fatalf("cross-project same name must not be flagged as duplicate: %+v", b.Duplicates)
	}

	// Re-remember within the same scope is idempotent (same identity).
	again, err := c.Remember(ctx, RememberInput{CanonicalName: "Config", Discriminators: map[string]string{"project": "goroutly"}})
	if err != nil {
		t.Fatalf("re-remember A: %v", err)
	}
	if again.Created || again.Node.ID != a.Node.ID {
		t.Fatalf("same-scope re-remember should hit the same node: created=%v id=%s", again.Created, again.Node.ID)
	}

	// A global Config (no discriminators) is yet another identity, not a dup.
	g, err := c.Remember(ctx, RememberInput{CanonicalName: "Config"})
	if err != nil {
		t.Fatalf("remember global: %v", err)
	}
	if !g.Created || len(g.Duplicates) != 0 {
		t.Fatalf("global Config: created=%v dups=%+v", g.Created, g.Duplicates)
	}

	// Scoped lookup returns the right node per scope.
	got, err := store.GetNodeByCanonicalNameScoped(ctx, pool, "Config", model.ScopeKey(map[string]string{"project": "brainiac"}))
	if err != nil || got == nil || got.ID != b.Node.ID {
		t.Fatalf("scoped lookup wrong: node=%+v err=%v", got, err)
	}
	if got.Discriminators["project"] != "brainiac" {
		t.Fatalf("discriminators not round-tripped: %+v", got.Discriminators)
	}

	// A second axis (env) distinguishes within the same project (#125).
	prod, err := c.Remember(ctx, RememberInput{CanonicalName: "Config", Discriminators: map[string]string{"project": "goroutly", "env": "prod"}})
	if err != nil {
		t.Fatalf("remember prod: %v", err)
	}
	if !prod.Created || prod.Node.ID == a.Node.ID {
		t.Fatalf("adding env axis must yield a new distinct node: created=%v", prod.Created)
	}

	// Invalid discriminators are rejected.
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "Bad", Discriminators: map[string]string{"env": "a;b"}}); err == nil {
		t.Fatal("discriminator with ';' should be rejected")
	}
}

func TestLinkCreatesNodesAndEdge(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	edge, err := c.Link(ctx, LinkInput{
		From: "OrderService", Type: "writes_to", To: "Postgres",
		Why: "orders are persisted", SourceURI: "repo://orders", Author: "claude",
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}

	from, err := store.GetNodeByCanonicalName(ctx, pool, "OrderService")
	if err != nil || from == nil {
		t.Fatalf("from node missing: %v", err)
	}
	edges, err := store.ListEdgesFrom(ctx, pool, from.ID)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 || edges[0].ID != edge.ID || edges[0].Why != "orders are persisted" {
		t.Fatalf("edge not persisted: %+v", edges)
	}
}

func TestLinkIsIdempotent(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	e1, err := c.Link(ctx, LinkInput{From: "A", Type: "writes_to", To: "B", Why: "first"})
	if err != nil {
		t.Fatalf("link 1: %v", err)
	}
	e2, err := c.Link(ctx, LinkInput{From: "A", Type: "writes_to", To: "B", Why: "second"})
	if err != nil {
		t.Fatalf("link 2: %v", err)
	}
	if e1.ID != e2.ID {
		t.Errorf("repeated link should return the same edge (%s vs %s)", e1.ID, e2.ID)
	}

	from, _ := store.GetNodeByCanonicalName(ctx, pool, "A")
	edges, err := store.ListEdgesFrom(ctx, pool, from.ID)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("want exactly 1 edge, got %d", len(edges))
	}
	if edges[0].Why != "second" {
		t.Errorf("rationale not refreshed on re-link: %q", edges[0].Why)
	}
}

func TestSearchReturnsNearestChunk(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	for _, text := range []string{"alpha", "beta", "gamma"} {
		emb, _ := hashEmbedder{}.Embed(ctx, text)
		if err := store.InsertChunk(ctx, pool, &model.Chunk{Text: text, Embedding: emb, SourceURI: "u/" + text}); err != nil {
			t.Fatalf("insert chunk %s: %v", text, err)
		}
	}

	hits, err := c.Search(ctx, "beta", 5, "")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// With the relevance cutoff, only the exact match (distance 0) is returned;
	// the orthogonal one-hot chunks (distance ~1.0) are dropped.
	if len(hits) != 1 || hits[0].Text != "beta" {
		t.Fatalf("expected only 'beta', got %+v", hits)
	}
}

func hasDup(dups []DuplicateCandidate, name, reason string) bool {
	for _, d := range dups {
		if d.Node.CanonicalName == name && d.Reason == reason {
			return true
		}
	}
	return false
}
