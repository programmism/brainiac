package store

import (
	"context"
	"time"
)

// PurgeHistoricalOlderThan hard-deletes aged historical rows (#363): a retention
// pass over the "keep everything" default so superseded churn doesn't grow
// unbounded. It only ever touches status='historical' rows whose valid-time
// (superseded_at) is known and older than cutoff — current rows, and historical
// rows retired before valid-time existed (superseded_at NULL, #200), are never
// affected.
//
// Order and scope keep it FK-safe:
//   - Historical EDGES first — nothing references an edge, so this is always safe.
//   - Then historical NODES that have NO remaining edge referencing them (from_id
//     or to_id), so a live supersedes edge pointing at an old node is never
//     orphaned. A still-referenced historical node is kept until its edges age out.
//
// Runs in the caller's transaction.
func PurgeHistoricalOlderThan(ctx context.Context, db DBTX, cutoff time.Time) (DeleteCounts, error) {
	var c DeleteCounts
	et, err := db.Exec(ctx,
		`DELETE FROM edges WHERE status = 'historical' AND superseded_at IS NOT NULL AND superseded_at < $1`, cutoff)
	if err != nil {
		return c, err
	}
	c.Edges = et.RowsAffected()

	nt, err := db.Exec(ctx, `
		DELETE FROM nodes n
		WHERE n.status = 'historical' AND n.superseded_at IS NOT NULL AND n.superseded_at < $1
		  AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.from_id = n.id OR e.to_id = n.id)`, cutoff)
	if err != nil {
		return c, err
	}
	c.Nodes = nt.RowsAffected()
	return c, nil
}
