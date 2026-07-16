package core

import (
	"context"
	"testing"
)

// The librarian must propose a near-duplicate the exact-name pass misses: two
// differently-named entities with (near-)identical summaries (#260). Under the
// hashEmbedder, identical summary text yields the same vector (distance 0).
func TestConsolidateProposesSemanticMergeCandidate(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ctx := context.Background()

	const summary = "the relational database we run in production"
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "Postgres", Summary: summary}); err != nil {
		t.Fatalf("remember 1: %v", err)
	}
	if _, err := c.Remember(ctx, RememberInput{CanonicalName: "PostgreSQL", Summary: summary}); err != nil {
		t.Fatalf("remember 2: %v", err)
	}

	rep, err := c.Consolidate(ctx, false)
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	var found bool
	for _, g := range rep.MergeGroups {
		var hasA, hasB bool
		for _, n := range g {
			hasA = hasA || n.CanonicalName == "Postgres"
			hasB = hasB || n.CanonicalName == "PostgreSQL"
		}
		if hasA && hasB {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a semantic merge candidate pairing Postgres + PostgreSQL, got %+v", rep.MergeGroups)
	}
}
