package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/store"
)

func TestConsolidationOrphanAndAgingSweeps(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// A linked pair (not orphan) and a lone node (orphan — no edges).
	if _, err := c.Link(ctx, LinkInput{From: "OrderService", Type: "writes-to", To: "Kafka", Why: "durability"}); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "LonelyEntity"}); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	rep, err := c.Consolidate(ctx, false)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}

	// Orphan sweep: the lone node surfaces; the linked pair does not.
	orphanNames := map[string]bool{}
	for _, n := range rep.Orphans {
		orphanNames[n.CanonicalName] = true
	}
	if !orphanNames["LonelyEntity"] {
		t.Errorf("LonelyEntity should be an orphan: %v", orphanNames)
	}
	if orphanNames["OrderService"] || orphanNames["Kafka"] {
		t.Errorf("linked nodes must not be orphans: %v", orphanNames)
	}

	// Aging sweep: fresh edge is not aging yet.
	if len(rep.Aging) != 0 {
		t.Errorf("a just-created edge must not be aging: %d", len(rep.Aging))
	}

	// Age the edge past EdgeStaleAge (never-confirmed → COALESCE falls to created_at).
	if _, err := pool.Exec(ctx, "UPDATE edges SET created_at = now() - interval '365 days', last_confirmed_at = NULL"); err != nil {
		t.Fatalf("age edge: %v", err)
	}
	rep2, err := c.Consolidate(ctx, false)
	if err != nil {
		t.Fatalf("consolidate 2: %v", err)
	}
	if len(rep2.Aging) != 1 {
		t.Fatalf("the aged edge should surface in Aging: %d", len(rep2.Aging))
	}
}

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
	report, err := c.Consolidate(ctx, true)
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
	report, _ = c.Consolidate(ctx, true)
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
	report, _ = c.Consolidate(ctx, true)
	if len(report.Stale) != 1 {
		t.Fatalf("stale = %d, want 1", len(report.Stale))
	}
	if err := c.Confirm(ctx, edges[0].ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	report, _ = c.Consolidate(ctx, true)
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

func TestDisambiguate(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// A node in project goroutly with an edge (a fact) attached.
	if _, err := c.Link(ctx, LinkInput{
		From: "Config", Type: "loaded_by", To: "Bootstrap", Why: "startup",
		Discriminators: map[string]string{"project": "goroutly"},
	}); err != nil {
		t.Fatalf("seed link: %v", err)
	}
	orig, err := store.GetNodeByCanonicalNameScoped(ctx, pool, "Config", "project=goroutly")
	if err != nil || orig == nil {
		t.Fatalf("seed node missing: %v", err)
	}

	// Realize it's the prod one → add env=prod. Node id and edges are preserved.
	got, err := c.Disambiguate(ctx, orig.ID, map[string]string{"env": "prod"})
	if err != nil {
		t.Fatalf("disambiguate: %v", err)
	}
	if got.ID != orig.ID {
		t.Fatalf("disambiguate should re-scope in place, id changed %s→%s", orig.ID, got.ID)
	}
	if got.Discriminators["project"] != "goroutly" || got.Discriminators["env"] != "prod" {
		t.Fatalf("merged discriminators wrong: %+v", got.Discriminators)
	}
	// Edge still attached to the (re-scoped) node.
	edges, err := store.ListEdgesFrom(ctx, pool, orig.ID)
	if err != nil || len(edges) != 1 {
		t.Fatalf("edge must survive re-scope: edges=%d err=%v", len(edges), err)
	}
	// It now lives at the new identity, not the old one.
	if n, _ := store.GetNodeByCanonicalNameScoped(ctx, pool, "Config", "project=goroutly"); n != nil {
		t.Fatalf("old scope should be empty after re-scope, got %s", n.ID)
	}
	if n, _ := store.GetNodeByCanonicalNameScoped(ctx, pool, "Config", "env=prod;project=goroutly"); n == nil || n.ID != orig.ID {
		t.Fatalf("node should be findable at new scope")
	}

	// A later save of the staging variant is a distinct entity.
	staging, err := c.Remember(ctx, RememberInput{CanonicalName: "Config", Discriminators: map[string]string{"project": "goroutly", "env": "staging"}})
	if err != nil {
		t.Fatalf("remember staging: %v", err)
	}
	if !staging.Created || staging.Node.ID == orig.ID {
		t.Fatalf("staging Config must be distinct: created=%v", staging.Created)
	}

	// Disambiguating into an occupied identity errors (points to merge).
	if _, err := c.Disambiguate(ctx, staging.Node.ID, map[string]string{"env": "prod"}); err == nil {
		t.Fatal("disambiguate into an occupied identity should error")
	}
}

func TestSplitTangledNode(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// A conflated Config: same relationship, two different targets (prod vs staging).
	disc := map[string]string{"project": "goroutly"}
	if _, err := c.Link(ctx, LinkInput{From: "Config", Type: "writes_to", To: "Kafka", Why: "prod bus", Discriminators: disc}); err != nil {
		t.Fatalf("link kafka: %v", err)
	}
	if _, err := c.Link(ctx, LinkInput{From: "Config", Type: "writes_to", To: "RabbitMQ", Why: "staging bus", Discriminators: disc}); err != nil {
		t.Fatalf("link rabbit: %v", err)
	}

	// The detector flags Config as a split candidate with its edges.
	rep, err := c.Consolidate(ctx, true)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	var cand *SplitCandidate
	for i := range rep.Splits {
		if rep.Splits[i].Node.CanonicalName == "Config" {
			cand = &rep.Splits[i]
		}
	}
	if cand == nil || len(cand.Edges) != 2 {
		t.Fatalf("expected Config split candidate with 2 edges, got %+v", rep.Splits)
	}

	// Route each edge by its target: Kafka→prod, RabbitMQ→staging.
	routes := map[string]string{}
	for _, e := range cand.Edges {
		switch e.ToName {
		case "Kafka":
			routes[e.Edge.ID] = "prod"
		case "RabbitMQ":
			routes[e.Edge.ID] = "staging"
		}
	}
	if len(routes) != 2 {
		t.Fatalf("could not route both edges: %+v", routes)
	}

	res, err := c.Split(ctx, cand.Node.ID, "env", routes)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if !res.ParentRetired || len(res.Children) != 2 {
		t.Fatalf("split result: retired=%v children=%d", res.ParentRetired, len(res.Children))
	}

	// Each child is a distinct scoped Config carrying one edge.
	prod, err := store.GetNodeByCanonicalNameScoped(ctx, pool, "Config", "env=prod;project=goroutly")
	if err != nil || prod == nil {
		t.Fatalf("prod child missing: %v", err)
	}
	staging, err := store.GetNodeByCanonicalNameScoped(ctx, pool, "Config", "env=staging;project=goroutly")
	if err != nil || staging == nil {
		t.Fatalf("staging child missing: %v", err)
	}
	if pe, _ := store.ListEdgesFrom(ctx, pool, prod.ID); len(pe) != 1 || pe[0].ToID == "" {
		t.Fatalf("prod child should carry exactly one edge, got %d", len(pe))
	}
	if se, _ := store.ListEdgesFrom(ctx, pool, staging.ID); len(se) != 1 {
		t.Fatalf("staging child should carry exactly one edge, got %d", len(se))
	}
	// The conflated parent is gone (retired), leaving no current edges.
	if pe, _ := store.ListEdgesFrom(ctx, pool, cand.Node.ID); len(pe) != 0 {
		t.Fatalf("parent should have no current edges after split, got %d", len(pe))
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

func TestRetireEdgeResolvesConflict(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Two current edges, same from+type, different targets → a conflict.
	mustLink(ctx, t, c, "PaymentService", "charges_via", "Stripe")
	mustLink(ctx, t, c, "PaymentService", "charges_via", "Adyen")

	rep, err := c.Consolidate(ctx, true)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	var conf *Conflict
	for i := range rep.Conflicts {
		if rep.Conflicts[i].From == "PaymentService" && rep.Conflicts[i].Type == "charges_via" {
			conf = &rep.Conflicts[i]
		}
	}
	if conf == nil {
		t.Fatalf("expected a charges_via conflict: %+v", rep.Conflicts)
	}
	if conf.EdgeA == "" || conf.EdgeB == "" {
		t.Fatalf("conflict must carry both edge ids: %+v", conf)
	}

	// Retire the losing edge → conflict resolved (one current edge remains).
	if err := c.RetireEdge(ctx, conf.EdgeB); err != nil {
		t.Fatalf("retire: %v", err)
	}
	rep2, err := c.Consolidate(ctx, true)
	if err != nil {
		t.Fatalf("re-consolidate: %v", err)
	}
	if hasConflict(rep2.Conflicts, "PaymentService", "charges_via") {
		t.Fatalf("conflict should be gone after retire: %+v", rep2.Conflicts)
	}

	// Replacement, not deletion: retired edge is historical but reachable via history.
	from, err := store.GetNodeByCanonicalName(ctx, pool, "PaymentService")
	if err != nil || from == nil {
		t.Fatalf("get node: %v", err)
	}
	currentEdges, err := store.EdgesForNode(ctx, pool, from.ID, false, 50, store.NoWall())
	if err != nil {
		t.Fatalf("current edges: %v", err)
	}
	for _, e := range currentEdges {
		if e.ID == conf.EdgeB {
			t.Fatalf("retired edge %s still current", conf.EdgeB)
		}
	}
	histEdges, err := store.EdgesForNode(ctx, pool, from.ID, true, 50, store.NoWall())
	if err != nil {
		t.Fatalf("edges incl history: %v", err)
	}
	var retired *model.Edge
	for i := range histEdges {
		if histEdges[i].ID == conf.EdgeB {
			retired = &histEdges[i]
		}
	}
	if retired == nil {
		t.Fatalf("retired edge not reachable via history")
	}
	if retired.Status != model.StatusHistorical {
		t.Fatalf("retired edge status = %s, want historical", retired.Status)
	}

	// A missing edge id is an error, not a silent no-op.
	if err := c.RetireEdge(ctx, "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Error("retiring a missing edge should error")
	}
}

func TestTypeNormalizationEnablesConflictDetection(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	// Same relationship intent written two different ways (#156): without
	// normalization these would be distinct types and the contradiction would
	// slip past conflict detection.
	e1, err := c.Link(ctx, LinkInput{From: "Billing", Type: "writes-to", To: "Kafka", Why: "x"})
	if err != nil {
		t.Fatalf("link 1: %v", err)
	}
	if _, err := c.Link(ctx, LinkInput{From: "Billing", Type: "writesTo", To: "RabbitMQ", Why: "x"}); err != nil {
		t.Fatalf("link 2: %v", err)
	}
	if e1.Type != "writes_to" {
		t.Fatalf("edge type = %q, want normalized writes_to", e1.Type)
	}

	rep, err := c.Consolidate(ctx, true)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if !hasConflict(rep.Conflicts, "Billing", "writes_to") {
		t.Fatalf("normalized types should surface the conflict: %+v", rep.Conflicts)
	}
}

func hasStaleEdge(es []model.Edge, id string) bool {
	for _, e := range es {
		if e.ID == id {
			return true
		}
	}
	return false
}

func TestConsolidateFlagsStaleBySource(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const src = "src://design-doc"
	edge, err := c.Link(ctx, LinkInput{From: "OrderService", Type: "writes_to", To: "Kafka", Why: "durability", SourceURI: src})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	// Age the edge so a later source counts as "changed since we recorded it".
	if _, err := pool.Exec(ctx, `UPDATE edges SET created_at = now() - interval '1 hour' WHERE id=$1`, edge.ID); err != nil {
		t.Fatalf("age edge: %v", err)
	}

	// A chunk from the same source, modified after the edge was recorded.
	emb, _ := hashEmbedder{}.Embed(ctx, "orderservice now writes to rabbitmq")
	modAt := time.Now().Add(-time.Minute)
	ch := &model.Chunk{Text: "OrderService now writes to RabbitMQ", Embedding: emb, SourceURI: src, Tier: model.TierHot, ContentHash: "h1", SourceModifiedAt: &modAt}
	if err := store.InsertChunk(ctx, pool, ch); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	// Consolidate auto-flags the edge as possibly stale (§8.3).
	report, err := c.Consolidate(ctx, true)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if !hasStaleEdge(report.Stale, edge.ID) {
		t.Fatalf("edge should be recency-flagged stale: %+v", report.Stale)
	}

	// Confirm it → a later consolidation must NOT re-flag (source unchanged since).
	if err := c.Confirm(ctx, edge.ID); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	report, err = c.Consolidate(ctx, true)
	if err != nil {
		t.Fatalf("re-consolidate: %v", err)
	}
	if hasStaleEdge(report.Stale, edge.ID) {
		t.Fatalf("confirmed edge re-flagged though source unchanged: %+v", report.Stale)
	}

	// But if the source changes AGAIN (after the confirmation), it re-flags.
	if _, err := pool.Exec(ctx, `UPDATE chunks SET source_modified_at = now() + interval '1 minute' WHERE source_uri=$1`, src); err != nil {
		t.Fatalf("bump source: %v", err)
	}
	report, err = c.Consolidate(ctx, true)
	if err != nil {
		t.Fatalf("consolidate after re-edit: %v", err)
	}
	if !hasStaleEdge(report.Stale, edge.ID) {
		t.Fatalf("edge should re-flag after source changed post-confirmation: %+v", report.Stale)
	}
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
