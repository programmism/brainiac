package core

import (
	"context"
	"hash/fnv"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/programmism/brainiac/internal/model"
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
	return New(pool, hashEmbedder{}), pool
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

	hits, err := c.Search(ctx, "beta", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Text != "beta" {
		t.Fatalf("expected 'beta' first, got %+v", hits)
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
