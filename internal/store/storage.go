package store

import (
	"context"
	"encoding/json"

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

// DeleteChunksBySourceURINotIn removes chunks of a source whose content hash is
// not in keepHashes — i.e. content that was edited away or deleted. With an
// empty keepHashes, all chunks for the source are removed.
func DeleteChunksBySourceURINotIn(ctx context.Context, db DBTX, uri string, keepHashes []string) (int64, error) {
	if keepHashes == nil {
		keepHashes = []string{}
	}
	tag, err := db.Exec(ctx,
		`DELETE FROM chunks WHERE source_uri = $1 AND (content_hash IS NULL OR content_hash <> ALL($2))`,
		uri, keepHashes)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
