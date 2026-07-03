package core

import (
	"context"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

func TestRecallComposesVectorAndGraph(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// A node with a summary embedding so it is retrievable by the query.
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "OrderService", Type: "service", Summary: "orders"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	// An edge with provenance.
	if _, err := c.Link(ctx, LinkInput{
		From: "OrderService", Type: "writes_to", To: "Postgres",
		Why: "orders persisted for durability", SourceURI: "doc://orders", Author: "claude",
	}); err != nil {
		t.Fatalf("link: %v", err)
	}
	// A chunk behind that provenance.
	emb, _ := hashEmbedder{}.Embed(ctx, "orders")
	if err := store.InsertChunk(ctx, pool, &model.Chunk{
		Text: "orders are persisted for durability", Embedding: emb, SourceURI: "doc://orders",
	}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	res, err := c.Recall(ctx, "orders", "")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res.Chunks) == 0 {
		t.Error("expected vector chunks")
	}
	if !containsNode(res.Nodes, "OrderService") {
		t.Errorf("expected OrderService in nodes: %+v", res.Nodes)
	}
	if !containsEdge(res.Edges, "OrderService", "Postgres", "writes_to") {
		t.Errorf("expected writes_to edge with names: %+v", res.Edges)
	}
	if len(res.EvidenceChunks) == 0 || res.EvidenceChunks[0].SourceURI != "doc://orders" {
		t.Errorf("expected evidence chunk by source_uri: %+v", res.EvidenceChunks)
	}
}

func TestSupersedeMarksHistoricalAndLinks(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	oldR, err := c.Remember(ctx, RememberInput{CanonicalName: "SyncPipeline"})
	if err != nil {
		t.Fatalf("remember old: %v", err)
	}
	newR, err := c.Remember(ctx, RememberInput{CanonicalName: "AsyncPipeline"})
	if err != nil {
		t.Fatalf("remember new: %v", err)
	}

	if err := c.Supersede(ctx, oldR.Node.ID, newR.Node.ID, "sync rejected due to peak load", "claude"); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	// Old node is now historical: a current-only lookup returns nothing.
	if got, _ := store.GetNodeByCanonicalName(ctx, pool, "SyncPipeline"); got != nil {
		t.Errorf("old node should be historical, got %+v", got)
	}
	old, err := store.GetNodeByID(ctx, pool, oldR.Node.ID)
	if err != nil || old == nil || old.Status != model.StatusHistorical {
		t.Fatalf("old node status: %+v (err %v)", old, err)
	}

	// A supersedes edge new -> old exists.
	edges, err := store.EdgesForNode(ctx, pool, newR.Node.ID, true, 50)
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	if !containsEdgeID(edges, newR.Node.ID, oldR.Node.ID, "supersedes") {
		t.Errorf("expected supersedes edge new->old: %+v", edges)
	}
}

func containsNode(nodes []model.Node, name string) bool {
	for _, n := range nodes {
		if n.CanonicalName == name {
			return true
		}
	}
	return false
}

func containsEdge(edges []EdgeView, from, to, typ string) bool {
	for _, e := range edges {
		if e.FromName == from && e.ToName == to && e.Edge.Type == typ {
			return true
		}
	}
	return false
}

func containsEdgeID(edges []model.Edge, fromID, toID, typ string) bool {
	for _, e := range edges {
		if e.FromID == fromID && e.ToID == toID && e.Type == typ {
			return true
		}
	}
	return false
}
