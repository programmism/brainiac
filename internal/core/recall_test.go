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

// TestRecallLexicalMentionAdmitsNamedEntity reproduces the precision bug with
// synthetic data: memory dominated by one domain plus a lone entity whose nickname
// is an alias. A query that names that entity by alias must surface it and its own
// edge — not the dominant domain's nodes/edges. Aliases aren't embedded, so with
// hashEmbedder every node is orthogonal to the query (distance 1 > MaxNodeDistance)
// and only the lexical path can reach the named entity.
func TestRecallLexicalMentionAdmitsNamedEntity(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Dominant domain: two services with an edge between them.
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "WidgetService", Type: "service", Summary: "assembles widgets"}); err != nil {
		t.Fatalf("remember widget: %v", err)
	}
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "GadgetStore", Type: "service", Summary: "stores gadgets"}); err != nil {
		t.Fatalf("remember gadget: %v", err)
	}
	if _, err := c.Link(ctx, LinkInput{From: "WidgetService", Type: "depends_on", To: "GadgetStore", Why: "widgets are built from gadgets", Author: "tester"}); err != nil {
		t.Fatalf("link services: %v", err)
	}

	// The lone named entity, reached only by alias.
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "Dana Quill", Type: "person", Aliases: []string{"Dizzy", "dquill"}, Summary: "keeps the lab tidy"}); err != nil {
		t.Fatalf("remember entity: %v", err)
	}
	if _, err := c.Link(ctx, LinkInput{From: "Dana Quill", Type: "member_of", To: "Blue Team", Why: "assigned at onboarding", Author: "tester"}); err != nil {
		t.Fatalf("link entity: %v", err)
	}

	res, err := c.Recall(ctx, "who is Dizzy?", "")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if !containsNode(res.Nodes, "Dana Quill") {
		t.Errorf("expected the entity admitted via its alias: %+v", res.Nodes)
	}
	if containsNode(res.Nodes, "WidgetService") || containsNode(res.Nodes, "GadgetStore") {
		t.Errorf("dominant-domain nodes leaked into an alias query: %+v", res.Nodes)
	}
	if !containsEdge(res.Edges, "Dana Quill", "Blue Team", "member_of") {
		t.Errorf("expected the entity's own edge: %+v", res.Edges)
	}
	if containsEdge(res.Edges, "WidgetService", "GadgetStore", "depends_on") {
		t.Errorf("dominant-domain edge flooded an alias query: %+v", res.Edges)
	}
}

// TestRecallForeignQueryReturnsEmptyNodes asserts a query with no relevant entity
// and no name/alias mention returns no nodes and no edges (a flat, off-corpus
// distance distribution must not be dredged up as confidently-cited noise).
func TestRecallForeignQueryReturnsEmptyNodes(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "WidgetService", Type: "service", Summary: "assembles widgets"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "GadgetStore", Type: "service", Summary: "stores gadgets"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := c.Link(ctx, LinkInput{From: "WidgetService", Type: "depends_on", To: "GadgetStore", Why: "widgets are built from gadgets", Author: "tester"}); err != nil {
		t.Fatalf("link: %v", err)
	}

	res, err := c.Recall(ctx, "sourdough bread baking recipe", "")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(res.Nodes) != 0 {
		t.Errorf("expected no nodes for a foreign query, got: %+v", res.Nodes)
	}
	if len(res.Edges) != 0 {
		t.Errorf("expected no edges for a foreign query, got: %+v", res.Edges)
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
