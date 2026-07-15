package core

import (
	"context"
	"errors"
	"testing"
)

// A principal must not mutate a node in a namespace it doesn't own, by id (#265).
func TestByIdMutationsRespectWall(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	ids := isoFixture(t, c, pool)
	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"}
	foreign := ids["Gamma"] // project B
	own := ids["Alpha"]     // project A

	if err := c.Supersede(ctxAs(a), foreign, own, "why", "t"); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("supersede of foreign node must be forbidden, got %v", err)
	}
	if _, err := c.Disambiguate(ctxAs(a), foreign, map[string]string{"env": "prod"}); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("disambiguate of foreign node must be forbidden, got %v", err)
	}
	if _, err := c.Split(ctxAs(a), foreign, "env", map[string]string{"e1": "prod"}); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("split of foreign node must be forbidden, got %v", err)
	}
	if err := c.ApplyMerge(ctxAs(a), own, foreign); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("merge dropping a foreign node must be forbidden, got %v", err)
	}
	// A principal must not re-scope the project axis of even its OWN node.
	if _, err := c.Disambiguate(ctxAs(a), own, map[string]string{"project": "C"}); !errors.Is(err, ErrForbiddenNamespace) {
		t.Fatalf("disambiguate changing project must be forbidden, got %v", err)
	}
	// An operator (no principal) can mutate across namespaces (no regression).
	if err := c.Supersede(context.Background(), foreign, own, "why", "t"); err != nil {
		t.Fatalf("operator supersede should succeed: %v", err)
	}
}
