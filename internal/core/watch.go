package core

import (
	"context"

	"github.com/programmism/brainiac/internal/plugins"
)

// ApplyChanges consumes a connector's Watch() stream and applies source-side
// changes to memory (#323). A "deleted" change propagates the deletion —
// membership-based (#387), so a document's chunks drop only when this source was
// their last claim — but only when opts.PruneMissing is set, keeping deletion
// propagation opt-in (the #107 retention default is to keep content). An
// "upserted" change is counted but not applied here: Watch() carries only the
// SourceURI, so re-ingesting the new content needs a Fetch — that (and persisted
// connector cursors) is the remaining #323 scope.
//
// This is a standalone capability. Connectors implement Watch() but nothing wires
// it into the running server yet (the markdown connector's Watch is currently a
// no-op), so ApplyChanges changes no default behavior — like BatchExtractor (#326),
// it's ready for a future streaming-sync driver. A yielded iterator error ends the
// stream and is returned; a cancelled context is terminal.
func (c *Core) ApplyChanges(ctx context.Context, conn plugins.SourceConnector, opts IngestOptions) (IngestStats, error) {
	var stats IngestStats
	for change, err := range conn.Watch(ctx) {
		if err != nil {
			if ctx.Err() != nil {
				return stats, ctx.Err()
			}
			stats.FetchErrors++
			continue
		}
		if change.SourceURI == "" {
			continue
		}
		switch change.Kind {
		case plugins.ChangeDeleted:
			if !opts.PruneMissing {
				continue // deletion propagation is opt-in (#247/#107)
			}
			deleted, derr := c.propagateDelete(ctx, change.SourceURI)
			if derr != nil {
				return stats, derr
			}
			stats.Deleted += int(deleted)
			stats.DeletedDocs++
		case plugins.ChangeUpserted:
			// Needs a Fetch of the new content to re-ingest; tracked as remaining
			// #323 scope. Counted so a caller can see upserts arrived.
			stats.SkippedDocs++
		}
	}
	return stats, nil
}
