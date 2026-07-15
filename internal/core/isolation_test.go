package core

import (
	"context"
	"errors"
	"testing"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

// isoFixture seeds two namespaces (A, B) and a global entity as a Layer-1
// operator (no principal), so the read/write-wall tests can then act as scoped
// principals over it. Returns the node ids by canonical name.
func isoFixture(t *testing.T, c *Core, pool store.DBTX) map[string]string {
	t.Helper()
	ctx := context.Background()
	ids := map[string]string{}
	remember := func(name, project, summary string) {
		in := RememberInput{CanonicalName: name, Type: "service", Summary: summary}
		if project != "" {
			in.Discriminators = map[string]string{"project": project}
		}
		r, err := c.Remember(ctx, in)
		if err != nil {
			t.Fatalf("seed remember %s: %v", name, err)
		}
		ids[name] = r.Node.ID
	}
	remember("Alpha", "A", "alpha apple")
	remember("Beta", "A", "beta banana")
	remember("Gamma", "B", "gamma grape")
	remember("Shared", "", "shared global note")

	// Same-namespace edges via Link (Link scopes both endpoints to its disc).
	if _, err := c.Link(ctx, LinkInput{From: "Alpha", Type: "writes_to", To: "Beta",
		Why: "a-internal", SourceURI: "doc://a", Discriminators: map[string]string{"project": "A"}}); err != nil {
		t.Fatalf("seed link A: %v", err)
	}
	if _, err := c.Link(ctx, LinkInput{From: "Gamma", Type: "writes_to", To: "Delta",
		Why: "b-internal", SourceURI: "doc://b", Discriminators: map[string]string{"project": "B"}}); err != nil {
		t.Fatalf("seed link B: %v", err)
	}
	// A cross-namespace edge (A → global) inserted directly, since Link can't span
	// scopes: it must be hidden from a principal that can't see both endpoints.
	if err := store.InsertEdge(ctx, pool, &model.Edge{
		FromID: ids["Alpha"], ToID: ids["Shared"], Type: "relates_to", Why: "cross"}); err != nil {
		t.Fatalf("seed cross edge: %v", err)
	}

	// Scoped chunks so Search/Recall can be probed per namespace.
	chunk := func(text, project string) {
		emb, _ := hashEmbedder{}.Embed(ctx, text)
		ch := &model.Chunk{Text: text, Embedding: emb, SourceURI: "doc://" + text}
		if project != "" {
			ch.Discriminators = map[string]string{"project": project}
		}
		if err := store.InsertChunk(ctx, pool, ch); err != nil {
			t.Fatalf("seed chunk %q: %v", text, err)
		}
	}
	chunk("alpha apple pie", "A")
	chunk("gamma grape jam", "B")
	chunk("shared global note text", "")
	return ids
}

func ctxAs(p *Principal) context.Context { return WithPrincipal(context.Background(), p) }

func TestHardIsolationReadWall(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ids := isoFixture(t, c, pool)

	a := &Principal{Name: "team-a", Read: []string{"A"}, Write: "A"}

	// Search: A sees its own chunk, never B's or global's.
	if hits, err := c.Search(ctxAs(a), "alpha apple pie", 10, ""); err != nil || len(hits) == 0 {
		t.Fatalf("A should find its own chunk: hits=%d err=%v", len(hits), err)
	}
	if hits, err := c.Search(ctxAs(a), "gamma grape jam", 10, ""); err != nil || len(hits) != 0 {
		t.Fatalf("A must not see B's chunk: hits=%d err=%v", len(hits), err)
	}
	if hits, err := c.Search(ctxAs(a), "shared global note text", 10, ""); err != nil || len(hits) != 0 {
		t.Fatalf("A must not see global chunk (no global in read-set): hits=%d err=%v", len(hits), err)
	}

	// Recall: probing B's entity by name returns nothing for A.
	res, err := c.Recall(ctxAs(a), "tell me about Gamma", "")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if containsNode(res.Nodes, "Gamma") {
		t.Fatalf("A recalled a B node: %+v", res.Nodes)
	}

	// get_node: by name and by id across the wall both read as not-found.
	if det, err := c.GetNode(ctxAs(a), "", "Gamma", ""); err != nil || det != nil {
		t.Fatalf("A get_node by B name must be nil: det=%v err=%v", det, err)
	}
	if det, err := c.GetNode(ctxAs(a), ids["Gamma"], "", ""); err != nil || det != nil {
		t.Fatalf("A get_node by B id must be nil (no leak): det=%v err=%v", det, err)
	}
	if det, err := c.GetNode(ctxAs(a), "", "Alpha", ""); err != nil || det == nil {
		t.Fatalf("A must resolve its own entity by name: det=%v err=%v", det, err)
	}

	// Graph: only A's nodes/edges; no B node, no cross-namespace edge.
	g, err := c.Graph(ctxAs(a), 200)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	for _, n := range g.Nodes {
		if n.Name == "Gamma" || n.Name == "Shared" {
			t.Fatalf("graph leaked out-of-namespace node %q", n.Name)
		}
	}
	for _, e := range g.Edges {
		if e.Type == "relates_to" {
			t.Fatalf("graph leaked the cross-namespace edge")
		}
	}
}

func TestHardIsolationGlobalDefault(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	isoFixture(t, c, pool)

	// No global in read-set → global invisible.
	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"}
	if hits, _ := c.Search(ctxAs(a), "shared global note text", 10, ""); len(hits) != 0 {
		t.Fatalf("global must not leak without explicit read: hits=%d", len(hits))
	}
	// Explicit global in read-set → global visible.
	ag := &Principal{Name: "a+g", Read: []string{"A", ""}, Write: "A"}
	if hits, _ := c.Search(ctxAs(ag), "shared global note text", 10, ""); len(hits) == 0 {
		t.Fatalf("global must be visible when read-set includes it")
	}
}

func TestHardIsolationNarrowCannotWiden(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	isoFixture(t, c, pool)

	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"}
	// Requesting project=B (outside the read-set) must return nothing, not B's data.
	if hits, err := c.Search(ctxAs(a), "gamma grape jam", 10, "B"); err != nil || len(hits) != 0 {
		t.Fatalf("?project=B must not widen past the wall: hits=%d err=%v", len(hits), err)
	}
}

func TestHardIsolationWritePin(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()
	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"}

	// A write naming another namespace is rejected, not silently redirected.
	if _, err := c.Remember(ctxAs(a), RememberInput{CanonicalName: "X",
		Discriminators: map[string]string{"project": "B"}}); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("remember into B must be forbidden, got %v", err)
	}
	if _, err := c.Link(ctxAs(a), LinkInput{From: "X", Type: "writes_to", To: "Y",
		Discriminators: map[string]string{"project": "B"}}); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("link into B must be forbidden, got %v", err)
	}

	// A bare write lands pinned in the principal's own namespace.
	if _, err := c.Remember(ctxAs(a), RememberInput{CanonicalName: "Pinned", Summary: "s"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if n, _ := store.GetNodeByCanonicalNameScoped(ctx, pool, "Pinned", model.ScopeKey(map[string]string{"project": "A"})); n == nil {
		t.Fatalf("bare write was not pinned to project=A")
	}
	// A non-project axis passes through, project is still pinned.
	if _, err := c.Remember(ctxAs(a), RememberInput{CanonicalName: "Env",
		Discriminators: map[string]string{"env": "prod"}}); err != nil {
		t.Fatalf("remember with env axis: %v", err)
	}
	if n, _ := store.GetNodeByCanonicalNameScoped(ctx, pool, "Env", model.ScopeKey(map[string]string{"project": "A", "env": "prod"})); n == nil {
		t.Fatalf("write with extra axis was not pinned to project=A;env=prod")
	}
}

func TestNilPrincipalIsLayer1(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ids := isoFixture(t, c, pool)

	// With no principal in context, reads are open (Layer 1) across all namespaces.
	if hits, _ := c.Search(context.Background(), "gamma grape jam", 10, ""); len(hits) == 0 {
		t.Fatalf("Layer 1 must see B chunk")
	}
	if hits, _ := c.Search(context.Background(), "shared global note text", 10, ""); len(hits) == 0 {
		t.Fatalf("Layer 1 must see global chunk")
	}
	// A B-scoped node resolves by id (a bare name resolves only the global scope in
	// Layer 1, unchanged) and by its explicit project.
	if det, err := c.GetNode(context.Background(), ids["Gamma"], "", ""); err != nil || det == nil {
		t.Fatalf("Layer 1 must resolve any node by id: det=%v err=%v", det, err)
	}
	if det, err := c.GetNode(context.Background(), "", "Gamma", "B"); err != nil || det == nil {
		t.Fatalf("Layer 1 must resolve a scoped node by project: det=%v err=%v", det, err)
	}
	g, err := c.Graph(context.Background(), 200)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	var sawGamma, sawShared bool
	for _, n := range g.Nodes {
		sawGamma = sawGamma || n.Name == "Gamma"
		sawShared = sawShared || n.Name == "Shared"
	}
	if !sawGamma || !sawShared {
		t.Fatalf("Layer 1 graph must include all namespaces: gamma=%v shared=%v", sawGamma, sawShared)
	}
}
