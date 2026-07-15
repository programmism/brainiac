package core

import (
	"context"
	"errors"
	"testing"
)

func TestExportNamespaceIsScoped(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	isoFixture(t, c, pool)
	ctx := context.Background()

	exp, err := c.ExportNamespace(ctx, "A")
	if err != nil {
		t.Fatalf("export A: %v", err)
	}
	// Only A's entities — never B's or global's.
	for _, n := range exp.Nodes {
		if n.CanonicalName == "Gamma" || n.CanonicalName == "Shared" {
			t.Fatalf("export A leaked node %q", n.CanonicalName)
		}
	}
	if !containsNode(exp.Nodes, "Alpha") || !containsNode(exp.Nodes, "Beta") {
		t.Fatalf("export A missing its own nodes: %+v", exp.Nodes)
	}
	// The cross-namespace edge (Alpha→Shared) is excluded (both-endpoints rule);
	// the internal A edge is present.
	for _, e := range exp.Edges {
		if e.Type == "relates_to" {
			t.Fatalf("export A leaked the cross-namespace edge")
		}
	}
	var sawInternal bool
	for _, e := range exp.Edges {
		sawInternal = sawInternal || e.Type == "writes_to"
	}
	if !sawInternal {
		t.Fatalf("export A missing its internal edge")
	}
	// Chunks: A's only.
	for _, ch := range exp.Chunks {
		if ch.Discriminators["project"] != "A" {
			t.Fatalf("export A leaked chunk in namespace %v", ch.Discriminators)
		}
	}
	if len(exp.Chunks) == 0 {
		t.Fatalf("export A missing its chunk")
	}
}

func TestExportNamespacePrincipalScoped(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	isoFixture(t, c, pool)

	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"}
	// A principal can export a namespace it may read...
	if _, err := c.ExportNamespace(ctxAs(a), "A"); err != nil {
		t.Fatalf("principal export of own namespace: %v", err)
	}
	// ...but not one outside its read-set.
	if _, err := c.ExportNamespace(ctxAs(a), "B"); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("principal export of foreign namespace must be forbidden, got %v", err)
	}
	// An empty namespace is an error, not a whole-DB dump.
	if _, err := c.ExportNamespace(context.Background(), ""); err == nil {
		t.Fatalf("empty namespace export must error")
	}
}
