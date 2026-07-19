package core

import (
	"context"
	"fmt"

	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

// BatchExtractor is the async batch-extraction API a strong extractor (Claude)
// offers (#383/#420): submit many chunks at once and poll for the results, ~50%
// cheaper than one synchronous request per chunk — for large backfills.
type BatchExtractor interface {
	// CreateBatch submits items and returns the provider's batch id without waiting.
	CreateBatch(ctx context.Context, items []plugins.BatchItem) (string, error)
	// FetchBatchResults checks once (non-blocking): (nil,false,nil) while the batch
	// is still processing, (results-by-custom_id, true, nil) once it has ended.
	FetchBatchResults(ctx context.Context, batchID string) (map[string]plugins.Extraction, bool, error)
}

// BatchWorkItem is one chunk to extract in a batch, with the context needed to
// apply its results later (#420).
type BatchWorkItem struct {
	CustomID       string
	Text           string
	SourceURI      string
	Discriminators map[string]string
	ForceReview    bool
}

// SubmitExtractionBatch submits the items as one provider batch and records the job
// + per-item context so a later poll can apply the results (#420). Returns the job
// id. No-op (empty id) when there are no items.
func (c *Core) SubmitExtractionBatch(ctx context.Context, items []BatchWorkItem) (string, error) {
	if c.batchExtractor == nil {
		return "", fmt.Errorf("batch extraction is not configured")
	}
	if len(items) == 0 {
		return "", nil
	}
	reqs := make([]plugins.BatchItem, len(items))
	for i, it := range items {
		reqs[i] = plugins.BatchItem{CustomID: it.CustomID, Text: it.Text}
	}
	providerID, err := c.batchExtractor.CreateBatch(ctx, reqs)
	if err != nil {
		return "", fmt.Errorf("create batch: %w", err)
	}
	var jobID string
	err = store.WithTx(ctx, c.pool, func(db store.DBTX) error {
		id, err := store.InsertExtractionBatch(ctx, db, providerID)
		if err != nil {
			return err
		}
		jobID = id
		recs := make([]store.ExtractionBatchItem, len(items))
		for i, it := range items {
			recs[i] = store.ExtractionBatchItem{
				CustomID: it.CustomID, SourceURI: it.SourceURI,
				Discriminators: it.Discriminators, ForceReview: it.ForceReview,
			}
		}
		return store.InsertBatchItems(ctx, db, jobID, recs)
	})
	if err != nil {
		return "", err
	}
	return jobID, nil
}

// PollExtractionBatches checks every submitted batch once and, for each that has
// ended, applies its succeeded results to the graph (via the same applyExtraction
// as the synchronous path, keyed by custom_id, honoring each item's scope/trust)
// and marks the job applied (#420). Returns how many jobs were applied. Best-effort
// per item — a missing/failed result is skipped, like a failed sync extraction.
func (c *Core) PollExtractionBatches(ctx context.Context) (applied int, err error) {
	if c.batchExtractor == nil {
		return 0, fmt.Errorf("batch extraction is not configured")
	}
	jobs, err := store.ExtractionBatchesByStatus(ctx, c.pool, store.BatchSubmitted)
	if err != nil {
		return 0, err
	}
	for _, job := range jobs {
		results, ended, err := c.batchExtractor.FetchBatchResults(ctx, job.ProviderBatchID)
		if err != nil {
			continue // transient — try again next poll
		}
		if !ended {
			continue
		}
		items, err := store.BatchItemsForJob(ctx, c.pool, job.ID)
		if err != nil {
			return applied, err
		}
		for cid, item := range items {
			ext, ok := results[cid]
			if !ok {
				continue // errored/expired at the provider → treat as failed extraction
			}
			if _, _, aerr := c.applyExtraction(ctx, ext, item.SourceURI, item.Discriminators, item.ForceReview); aerr != nil {
				c.extractFailures.Add(1)
			}
		}
		if err := store.SetExtractionBatchStatus(ctx, c.pool, job.ID, store.BatchApplied); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}
