package core

import (
	"context"
	"testing"
)

func TestAuditLogRecordsWrites(t *testing.T) {
	c, pool := newTestCore(t)
	defer pool.Close()

	// An operator (no principal) write.
	if _, err := c.Remember(context.Background(), RememberInput{CanonicalName: "Alpha"}); err != nil {
		t.Fatalf("remember operator: %v", err)
	}
	// A scoped principal write.
	a := &Principal{Name: "team-a", Read: []string{"A"}, Write: "A"}
	if _, err := c.Remember(ctxAs(a), RememberInput{CanonicalName: "Beta"}); err != nil {
		t.Fatalf("remember principal: %v", err)
	}

	entries, err := c.AuditLog(context.Background(), 10)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected >=2 audit entries, got %d", len(entries))
	}
	// Newest first: Beta by team-a in namespace A, then Alpha by operator.
	if entries[0].Operation != "remember" || entries[0].Principal != "team-a" || entries[0].Target != "Beta" || entries[0].Namespace != "A" {
		t.Fatalf("newest entry wrong: %+v", entries[0])
	}
	var sawOperatorAlpha bool
	for _, e := range entries {
		if e.Principal == "operator" && e.Target == "Alpha" {
			sawOperatorAlpha = true
		}
	}
	if !sawOperatorAlpha {
		t.Fatalf("missing operator Alpha entry: %+v", entries)
	}
}
