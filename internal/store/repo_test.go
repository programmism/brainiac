package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/programmism/brainiac/internal/model"
)

// vec returns a 768-dim unit vector with 1.0 at pos — distinct directions so
// cosine distance is meaningful.
func vec(pos int) []float32 {
	v := make([]float32, 768)
	v[pos] = 1
	return v
}

func TestRepositories(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping DB-backed repository test")
	}
	ctx := context.Background()

	pool, err := Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE edges, nodes, chunks"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Atomic node+node+edge insert.
	var aID, bID string
	err = WithTx(ctx, pool, func(db DBTX) error {
		a := &model.Node{CanonicalName: "OrderService", Type: "service", Aliases: []string{"Order Service"}}
		if err := InsertNode(ctx, db, a); err != nil {
			return err
		}
		b := &model.Node{CanonicalName: "Postgres", Type: "datastore"}
		if err := InsertNode(ctx, db, b); err != nil {
			return err
		}
		aID, bID = a.ID, b.ID
		return InsertEdge(ctx, db, &model.Edge{
			FromID: a.ID, ToID: b.ID, Type: "writes_to",
			Why: "orders persisted here", SourceURI: "repo://orders", Author: "claude",
		})
	})
	if err != nil {
		t.Fatalf("tx insert: %v", err)
	}

	// Lookup + aliases round-trip.
	got, err := GetNodeByCanonicalName(ctx, pool, "OrderService")
	if err != nil || got == nil {
		t.Fatalf("get node: %v (nil=%v)", err, got == nil)
	}
	if got.ID != aID || len(got.Aliases) != 1 || got.Aliases[0] != "Order Service" {
		t.Fatalf("node mismatch: %+v", got)
	}

	// Edge traversal + why/provenance preserved.
	edges, err := ListEdgesFrom(ctx, pool, aID)
	if err != nil {
		t.Fatalf("list edges: %v", err)
	}
	if len(edges) != 1 || edges[0].ToID != bID || edges[0].Why != "orders persisted here" || edges[0].Author != "claude" {
		t.Fatalf("edge mismatch: %+v", edges)
	}

	// Vector search returns the nearest chunk first.
	for _, c := range []struct {
		text string
		pos  int
		uri  string
	}{{"alpha", 0, "u0"}, {"beta", 5, "u5"}, {"gamma", 10, "u10"}} {
		if err := InsertChunk(ctx, pool, &model.Chunk{
			Text: c.text, Embedding: vec(c.pos), SourceURI: c.uri,
			SourceLocator: map[string]any{"pos": c.pos}, QualityScore: 0.9,
		}); err != nil {
			t.Fatalf("insert chunk %s: %v", c.text, err)
		}
	}
	hits, err := SearchChunks(ctx, pool, vec(5), 2, AllScopes())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 || hits[0].Text != "beta" {
		t.Fatalf("search mismatch: got %d hits, first=%v", len(hits), firstText(hits))
	}
	if hits[0].SourceLocator["pos"].(float64) != 5 {
		t.Fatalf("locator not round-tripped: %+v", hits[0].SourceLocator)
	}

	// Rollback: a failed tx leaves nothing behind.
	_ = WithTx(ctx, pool, func(db DBTX) error {
		if err := InsertNode(ctx, db, &model.Node{CanonicalName: "Ghost"}); err != nil {
			return err
		}
		return errors.New("boom")
	})
	ghost, err := GetNodeByCanonicalName(ctx, pool, "Ghost")
	if err != nil {
		t.Fatalf("get ghost: %v", err)
	}
	if ghost != nil {
		t.Fatal("rollback failed: Ghost node persisted")
	}
}

func firstText(hits []model.ChunkHit) string {
	if len(hits) == 0 {
		return "<none>"
	}
	return hits[0].Text
}
