package core

import (
	"errors"
	"testing"
)

func TestNodeQuota(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A", MaxNodes: 2}

	if _, err := c.Remember(ctxAs(a), RememberInput{CanonicalName: "N1"}); err != nil {
		t.Fatalf("remember 1: %v", err)
	}
	if _, err := c.Remember(ctxAs(a), RememberInput{CanonicalName: "N2"}); err != nil {
		t.Fatalf("remember 2: %v", err)
	}
	// At the cap, a new node is rejected.
	if _, err := c.Remember(ctxAs(a), RememberInput{CanonicalName: "N3"}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("remember at cap should be quota-exceeded, got %v", err)
	}
	// A link that would create a NEW node is rejected too.
	if _, err := c.Link(ctxAs(a), LinkInput{From: "N1", Type: "writes_to", To: "N4"}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("link creating a new node at cap should be quota-exceeded, got %v", err)
	}
	// A link between two EXISTING nodes creates nothing, so it still succeeds.
	if _, err := c.Link(ctxAs(a), LinkInput{From: "N1", Type: "writes_to", To: "N2"}); err != nil {
		t.Fatalf("link between existing nodes at cap should succeed: %v", err)
	}
}

func TestChunkQuota(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A", MaxChunks: 1}

	if _, err := c.IngestText(ctxAs(a), "u1", "OrderService streams events to Kafka for durability and audit.", ""); err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	// A second document would push the namespace past its chunk cap.
	if _, err := c.IngestText(ctxAs(a), "u2", "BillingService reconciles invoices against the ledger nightly.", ""); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("ingest past cap should be quota-exceeded, got %v", err)
	}
}

func TestNoQuotaIsUnlimited(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()
	a := &Principal{Name: "a", Read: []string{"A"}, Write: "A"} // MaxNodes/MaxChunks 0

	for i, name := range []string{"a", "b", "c", "d"} {
		if _, err := c.Remember(ctxAs(a), RememberInput{CanonicalName: name}); err != nil {
			t.Fatalf("remember %d with no quota should succeed: %v", i, err)
		}
	}
}
