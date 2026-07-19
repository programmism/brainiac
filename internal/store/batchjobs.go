package store

import (
	"context"
	"encoding/json"
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

// ExtractionBatchItem is the per-item context needed to apply a batch result
// (#420): where to attach the extracted graph and under what trust/scope.
type ExtractionBatchItem struct {
	CustomID       string
	SourceURI      string
	Discriminators map[string]string
	ForceReview    bool
}

// InsertBatchItems records the per-chunk context for a submitted batch.
func InsertBatchItems(ctx context.Context, db DBTX, batchID string, items []ExtractionBatchItem) error {
	for _, it := range items {
		disc := it.Discriminators
		if disc == nil {
			disc = map[string]string{}
		}
		discJSON, err := json.Marshal(disc)
		if err != nil {
			return err
		}
		if _, err := db.Exec(ctx,
			`INSERT INTO extraction_batch_items (batch_id, custom_id, source_uri, discriminators, force_review)
			 VALUES ($1, $2, $3, $4::jsonb, $5) ON CONFLICT DO NOTHING`,
			batchID, it.CustomID, it.SourceURI, discJSON, it.ForceReview); err != nil {
			return err
		}
	}
	return nil
}

// BatchItemsForJob returns a batch's items, keyed by custom_id, for the poller.
func BatchItemsForJob(ctx context.Context, db DBTX, batchID string) (map[string]ExtractionBatchItem, error) {
	rows, err := db.Query(ctx,
		`SELECT custom_id, source_uri, discriminators, force_review FROM extraction_batch_items WHERE batch_id = $1`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ExtractionBatchItem{}
	for rows.Next() {
		var it ExtractionBatchItem
		var disc []byte
		if err := rows.Scan(&it.CustomID, &it.SourceURI, &disc, &it.ForceReview); err != nil {
			return nil, err
		}
		it.Discriminators = decodeDiscriminators(disc)
		out[it.CustomID] = it
	}
	return out, rows.Err()
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
