package store

import (
	"context"

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

// SetChunkTier moves a chunk between the hot index and the cold archive.
func SetChunkTier(ctx context.Context, db DBTX, id string, tier model.Tier) error {
	_, err := db.Exec(ctx, `UPDATE chunks SET tier = $2 WHERE id = $1`, id, string(tier))
	return err
}
