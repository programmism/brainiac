package store

import (
	"context"
	"time"
)

// ExtractionBatch is one submitted Claude Message Batch tracked for async
// extraction (#383): the provider's batch id and its lifecycle status.
type ExtractionBatch struct {
	ID              string
	ProviderBatchID string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Extraction-batch lifecycle statuses.
const (
	BatchSubmitted = "submitted" // created at the provider, awaiting results
	BatchEnded     = "ended"     // provider finished; results fetched, not yet applied
	BatchApplied   = "applied"   // results turned into nodes/edges
	BatchFailed    = "failed"    // gave up (provider error / expired)
)

// InsertExtractionBatch records a newly submitted batch and returns its row id.
func InsertExtractionBatch(ctx context.Context, db DBTX, providerBatchID string) (string, error) {
	var id string
	err := db.QueryRow(ctx,
		`INSERT INTO extraction_batches (provider_batch_id) VALUES ($1) RETURNING id`,
		providerBatchID).Scan(&id)
	return id, err
}

// SetExtractionBatchStatus advances a batch's lifecycle status.
func SetExtractionBatchStatus(ctx context.Context, db DBTX, id, status string) error {
	_, err := db.Exec(ctx,
		`UPDATE extraction_batches SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	return err
}

// ExtractionBatchesByStatus returns the batches in a given lifecycle status,
// oldest first — the poller's work queue.
func ExtractionBatchesByStatus(ctx context.Context, db DBTX, status string) ([]ExtractionBatch, error) {
	rows, err := db.Query(ctx,
		`SELECT id, provider_batch_id, status, created_at, updated_at
		 FROM extraction_batches WHERE status = $1 ORDER BY created_at`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExtractionBatch
	for rows.Next() {
		var b ExtractionBatch
		if err := rows.Scan(&b.ID, &b.ProviderBatchID, &b.Status, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
