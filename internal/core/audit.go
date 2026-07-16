package core

import (
	"context"

	"github.com/programmism/brainiac/internal/store"
)

// audit records a write to the append-only audit log (#267). Best-effort: an
// audit failure is swallowed so it never fails the operation it records. The
// principal is the caller's name, or "operator" for an unscoped (Layer 1) write.
func (c *Core) audit(ctx context.Context, operation, target, namespace string) {
	who := "operator"
	if p := PrincipalFrom(ctx); p != nil {
		who = p.Name
	}
	_ = store.InsertAuditEntry(ctx, c.pool, store.AuditEntry{
		Principal: who, Operation: operation, Target: target, Namespace: namespace,
	})
}

// AuditLog returns the most recent audit entries (operator/read surface, #267).
func (c *Core) AuditLog(ctx context.Context, limit int) ([]store.AuditEntry, error) {
	return store.RecentAuditEntries(ctx, c.pool, limit)
}
