package core

import (
	"context"
	"fmt"
	"testing"

	"github.com/programmism/brainiac/internal/store"
)

func TestConsolidationFlow(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Two normalized-duplicate nodes → one merge group.
	keep, err := c.Remember(ctx, RememberInput{CanonicalName: "OrderService"})
	if err != nil {
		t.Fatalf("remember keep: %v", err)
	}
	drop, err := c.Remember(ctx, RememberInput{CanonicalName: "Order Service"})
	if err != nil {
		t.Fatalf("remember drop: %v", err)
	}
	merges, err := c.ProposeMerges(ctx)
	if err != nil {
		t.Fatalf("propose merges: %v", err)
	}
	if len(merges) != 1 || len(merges[0]) != 2 {
		t.Fatalf("merge groups = %+v", merges)
	}

	// Conflict: same source + type, two targets.
	mustLink(ctx, t, c, "OrderService", "writes_to", "Kafka")
	mustLink(ctx, t, c, "OrderService", "writes_to", "RabbitMQ")
	report, err := c.Consolidate(ctx)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if !hasConflict(report.Conflicts, "OrderService", "writes_to") {
		t.Errorf("expected writes_to conflict: %+v", report.Conflicts)
	}

	// Rollup: give the node ≥ RollupMinEdges edges.
	for i := 0; i < RollupMinEdges; i++ {
		mustLink(ctx, t, c, "OrderService", "depends_on", fmt.Sprintf("Svc%d", i))
	}
	report, _ = c.Consolidate(ctx)
	if !hasRollup(report.Rollups, "OrderService") {
		t.Errorf("expected OrderService rollup: %+v", report.Rollups)
	}

	// Stale flag lifecycle.
	edges, err := store.ListEdgesFrom(ctx, pool, keep.Node.ID)
	if err != nil || len(edges) == 0 {
		t.Fatalf("list edges: %v", err)
	}
	if err := c.FlagStale(ctx, edges[0].ID); err != nil {
		t.Fatalf("flag stale: %v", err)
	}
	report, _ = c.Consolidate(ctx)
	if len(report.Stale) != 1 {
		t.Fatalf("stale = %d, want 1", len(report.Stale))
	}
	if err := c.Confirm(ctx, edges[0].ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	report, _ = c.Consolidate(ctx)
	if len(report.Stale) != 0 {
		t.Fatalf("stale after confirm = %d, want 0", len(report.Stale))
	}

	// Apply merge: drop folds into keep.
	if err := c.ApplyMerge(ctx, keep.Node.ID, drop.Node.ID); err != nil {
		t.Fatalf("apply merge: %v", err)
	}
	if n, _ := store.GetNodeByCanonicalName(ctx, pool, "Order Service"); n != nil {
		t.Errorf("dropped node should be historical, got %+v", n)
	}
	merged, _ := store.GetNodeByID(ctx, pool, keep.Node.ID)
	if !containsStr(merged.Aliases, "Order Service") {
		t.Errorf("keep aliases should include 'Order Service': %+v", merged.Aliases)
	}
}

func TestProposeMergesRespectsScope(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Same normalized name in two different projects → NOT a merge candidate.
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "OrderService", Discriminators: map[string]string{"project": "alpha"}}); err != nil {
		t.Fatalf("remember alpha: %v", err)
	}
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "Order Service", Discriminators: map[string]string{"project": "beta"}}); err != nil {
		t.Fatalf("remember beta: %v", err)
	}
	merges, err := c.ProposeMerges(ctx)
	if err != nil {
		t.Fatalf("propose merges: %v", err)
	}
	if len(merges) != 0 {
		t.Fatalf("cross-project same-name must not be proposed for merge: %+v", merges)
	}

	// Two normalized-duplicates within the SAME project → one merge group.
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "PayService", Discriminators: map[string]string{"project": "alpha"}}); err != nil {
		t.Fatalf("remember pay1: %v", err)
	}
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "Pay Service", Discriminators: map[string]string{"project": "alpha"}}); err != nil {
		t.Fatalf("remember pay2: %v", err)
	}
	merges, err = c.ProposeMerges(ctx)
	if err != nil {
		t.Fatalf("propose merges 2: %v", err)
	}
	if len(merges) != 1 || len(merges[0]) != 2 {
		t.Fatalf("same-project duplicates should form one group of 2: %+v", merges)
	}
	if merges[0][0].Discriminators["project"] != "alpha" {
		t.Fatalf("merge group should be within project alpha: %+v", merges[0][0].Discriminators)
	}
}

func mustLink(ctx context.Context, t *testing.T, c *Core, from, typ, to string) {
	t.Helper()
	if _, err := c.Link(ctx, LinkInput{From: from, Type: typ, To: to, Why: "x"}); err != nil {
		t.Fatalf("link %s-%s->%s: %v", from, typ, to, err)
	}
}

func hasConflict(cs []Conflict, from, typ string) bool {
	for _, c := range cs {
		if c.From == from && c.Type == typ {
			return true
		}
	}
	return false
}

func hasRollup(rs []store.RollupCandidate, name string) bool {
	for _, r := range rs {
		if r.Name == name {
			return true
		}
	}
	return false
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
