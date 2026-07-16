package store

import (
	"context"
	"time"
)

// AuditEntry is one recorded write: who did what, to what, where, when (#267).
type AuditEntry struct {
	At        time.Time `json:"at"`
	Principal string    `json:"principal"`
	Operation string    `json:"operation"`
	Target    string    `json:"target,omitempty"`
	Namespace string    `json:"namespace,omitempty"`
}

// InsertAuditEntry appends one audit row. Best-effort at the call site: an audit
// failure must never fail the operation it records.
func InsertAuditEntry(ctx context.Context, db DBTX, e AuditEntry) error {
	_, err := db.Exec(ctx,
		`INSERT INTO audit_log (principal, operation, target, namespace) VALUES ($1, $2, $3, $4)`,
		e.Principal, e.Operation, nullStr(e.Target), nullStr(e.Namespace))
	return err
}

// RecentAuditEntries returns the most recent audit rows, newest first.
func RecentAuditEntries(ctx context.Context, db DBTX, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(ctx,
		`SELECT at, principal, operation, target, namespace FROM audit_log ORDER BY at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var target, ns *string
		if err := rows.Scan(&e.At, &e.Principal, &e.Operation, &target, &ns); err != nil {
			return nil, err
		}
		e.Target = deref(target)
		e.Namespace = deref(ns)
		out = append(out, e)
	}
	return out, rows.Err()
}
