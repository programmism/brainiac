package store

import "context"

// Multi-source provenance (#244). chunk_sources records which sources a chunk
// belongs to, so identical content ingested from several sources is one chunk
// with several memberships — and a source deletion drops the chunk only when it
// was the LAST source. These helpers are the membership primitives; wiring the
// ingest reconcile + global content dedup onto them is follow-up #387, so today
// they only keep membership current (RecordChunkSource, called by InsertChunk)
// without changing any read or delete behavior.

// RecordChunkSource records that a chunk belongs to a source. Idempotent — a
// (chunk_id, source_uri) already present is a no-op, so re-ingest of the same
// source is safe.
func RecordChunkSource(ctx context.Context, db DBTX, chunkID, sourceURI string) error {
	_, err := db.Exec(ctx,
		`INSERT INTO chunk_sources (chunk_id, source_uri) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		chunkID, sourceURI)
	return err
}

// ChunkSourceURIs returns the sources a chunk belongs to, sorted. Used by the
// membership-based prune and tests to reason about how many sources still vouch
// for a chunk.
func ChunkSourceURIs(ctx context.Context, db DBTX, chunkID string) ([]string, error) {
	rows, err := db.Query(ctx,
		`SELECT source_uri FROM chunk_sources WHERE chunk_id = $1 ORDER BY source_uri`, chunkID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DropChunkSourceMembershipNotIn removes a source's membership for chunks whose
// content_hash is not in keepHashes: content the source no longer carries (edited
// away or deleted) stops being vouched for by this source, but the chunk itself is
// NOT deleted here — another source may still vouch for it. Call PruneOrphanChunks
// afterward to remove chunks that lost their last source. Empty keepHashes drops
// all of the source's memberships. Returns the number of memberships removed. This
// is the reconcile deletion path (ingest.go); it replaced the old per-source chunk
// delete once membership became the source of truth (#387).
func DropChunkSourceMembershipNotIn(ctx context.Context, db DBTX, uri string, keepHashes []string) (int64, error) {
	if keepHashes == nil {
		keepHashes = []string{}
	}
	tag, err := db.Exec(ctx, `
		DELETE FROM chunk_sources cs
		USING chunks c
		WHERE cs.chunk_id = c.id
		  AND cs.source_uri = $1
		  AND (c.content_hash IS NULL OR c.content_hash <> ALL($2))`,
		uri, keepHashes)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PruneOrphanChunks deletes chunks that no longer belong to any source — the
// membership-based counterpart to per-source deletion: a chunk survives as long
// as one source still vouches for it, and is removed only when its last
// membership is gone. Returns the number of chunks deleted.
func PruneOrphanChunks(ctx context.Context, db DBTX) (int64, error) {
	tag, err := db.Exec(ctx,
		`DELETE FROM chunks c WHERE NOT EXISTS (SELECT 1 FROM chunk_sources cs WHERE cs.chunk_id = c.id)`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
