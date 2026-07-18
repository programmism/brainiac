package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/programmism/brainiac/internal/model"
)

// ChunkText is a chunk's id and raw text, for re-embedding.
type ChunkText struct {
	ID   string
	Text string
}

// AllChunkTexts returns every chunk's id and raw text. Raw text is always
// stored, so vectors can be rebuilt on an embedding-model change without
// re-reading sources (§13.5).
func AllChunkTexts(ctx context.Context, db DBTX) ([]ChunkText, error) {
	rows, err := db.Query(ctx, `SELECT id, text FROM chunks ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChunkText
	for rows.Next() {
		var c ChunkText
		if err := rows.Scan(&c.ID, &c.Text); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateChunkEmbedding replaces a chunk's embedding (text cast to halfvec).
func UpdateChunkEmbedding(ctx context.Context, db DBTX, id string, emb []float32) error {
	_, err := db.Exec(ctx, `UPDATE chunks SET embedding = $2::halfvec WHERE id = $1`, id, encodeVec(emb))
	return err
}

// UpdateChunkScope rewrites a chunk's identity scope (discriminators + scope_key)
// in place — the chunk-side mirror of UpdateNodeScope, used by a namespace handoff
// to move a chunk from one project to another (#188).
func UpdateChunkScope(ctx context.Context, db DBTX, id string, disc map[string]string) error {
	if disc == nil {
		disc = map[string]string{}
	}
	discJSON, err := json.Marshal(disc)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `UPDATE chunks SET discriminators = $2::jsonb, scope_key = $3 WHERE id = $1`,
		id, discJSON, model.ScopeKey(disc))
	return err
}

// SetChunkTier moves a chunk between the hot index and the cold archive.
func SetChunkTier(ctx context.Context, db DBTX, id string, tier model.Tier) error {
	_, err := db.Exec(ctx, `UPDATE chunks SET tier = $2 WHERE id = $1`, id, string(tier))
	return err
}

// DemoteStaleHotChunks archives every hot chunk created before olderThan to the
// cold tier (#231): the automated hot→cold half of tiering that keeps the hot
// vector index within RAM. Cold chunks keep their text + embedding (recoverable by
// re-promotion / reindex), they just leave the default hot search path. Returns
// how many were demoted.
func DemoteStaleHotChunks(ctx context.Context, db DBTX, olderThan time.Time) (int64, error) {
	tag, err := db.Exec(ctx,
		`UPDATE chunks SET tier = 'cold' WHERE tier = 'hot' AND created_at < $1`, olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ChunkHashesBySourceURI returns the set of content hashes currently stored for
// a source, so re-ingest can keep unchanged chunks and reconcile the rest.
func ChunkHashesBySourceURI(ctx context.Context, db DBTX, uri string) (map[string]bool, error) {
	rows, err := db.Query(ctx, `SELECT content_hash FROM chunks WHERE source_uri = $1 AND content_hash IS NOT NULL`, uri)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = true
	}
	return out, rows.Err()
}

// SourceSyncModifiedAt returns the last-synced source modification time recorded
// for a source_uri (#236), and false if none is stored yet.
func SourceSyncModifiedAt(ctx context.Context, db DBTX, uri string) (time.Time, bool, error) {
	var t *time.Time
	err := db.QueryRow(ctx, `SELECT modified_at FROM source_sync WHERE source_uri = $1`, uri).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return *t, true, nil
}

// UpsertSourceSync records that a source_uri was synced now, with the source's
// modification time (nil = unknown), so a later incremental run can skip it if it
// hasn't advanced (#236).
func UpsertSourceSync(ctx context.Context, db DBTX, uri string, modifiedAt *time.Time) error {
	_, err := db.Exec(ctx,
		`INSERT INTO source_sync (source_uri, modified_at, synced_at) VALUES ($1, $2, now())
		 ON CONFLICT (source_uri) DO UPDATE SET modified_at = EXCLUDED.modified_at, synced_at = now()`,
		uri, modifiedAt)
	return err
}
