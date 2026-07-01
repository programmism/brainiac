package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/programmism/brainiac/internal/model"
	"github.com/programmism/brainiac/internal/plugins"
	"github.com/programmism/brainiac/internal/store"
)

// DefaultChunkSize is the target chunk length in characters.
const DefaultChunkSize = 1000

// IngestOptions tunes an ingest run.
type IngestOptions struct {
	ChunkSize int
}

// IngestStats reports what happened during an ingest run.
type IngestStats struct {
	Docs    int // documents fetched
	Chunks  int // chunks seen
	Kept    int // stored hot
	Queued  int // stored cold (borderline; excluded from default search)
	Dropped int // rejected by the selector
	Skipped int // unchanged (content hash already present)
}

// Ingest runs the Layer-1 pipeline for a connector: fetch → chunk → select →
// embed → store (SYSTEM.md §8, PRD §8). Selection happens per-chunk and before
// the vector index; unchanged chunks (by content hash) are skipped.
func (c *Core) Ingest(ctx context.Context, conn plugins.SourceConnector, opts IngestOptions) (IngestStats, error) {
	if c.selector == nil {
		return IngestStats{}, fmt.Errorf("ingest requires a selector")
	}
	size := opts.ChunkSize
	if size <= 0 {
		size = DefaultChunkSize
	}

	var stats IngestStats
	for doc, err := range conn.Fetch(ctx) {
		if err != nil {
			return stats, fmt.Errorf("fetch: %w", err)
		}
		stats.Docs++
		for _, ck := range chunkText(doc.Text, size) {
			stats.Chunks++
			hash := hashText(ck)

			exists, err := store.ChunkExistsByHash(ctx, c.pool, hash)
			if err != nil {
				return stats, err
			}
			if exists {
				stats.Skipped++
				continue
			}

			score := c.selector.Score(ck)
			if score.Decision == plugins.Drop {
				stats.Dropped++
				continue
			}

			emb, err := c.embedder.Embed(ctx, ck)
			if err != nil {
				return stats, fmt.Errorf("embed chunk: %w", err)
			}

			tier := model.TierHot
			if score.Decision == plugins.Queue {
				tier = model.TierCold // borderline: kept but out of default search
				stats.Queued++
			} else {
				stats.Kept++
			}

			if err := store.InsertChunk(ctx, c.pool, &model.Chunk{
				Text:          ck,
				Embedding:     emb,
				SourceURI:     doc.SourceURI,
				SourceLocator: doc.SourceLocator,
				QualityScore:  score.Quality,
				Tier:          tier,
				ContentHash:   hash,
			}); err != nil {
				return stats, fmt.Errorf("store chunk: %w", err)
			}
		}
	}
	return stats, nil
}

// chunkText splits text into chunks of roughly size characters, packing whole
// paragraphs where possible and hard-splitting any paragraph longer than size.
func chunkText(text string, size int) []string {
	paras := strings.Split(text, "\n\n")
	var chunks []string
	var b strings.Builder

	flush := func() {
		if b.Len() > 0 {
			chunks = append(chunks, strings.TrimSpace(b.String()))
			b.Reset()
		}
	}

	for _, p := range paras {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for len(p) > size {
			flush()
			chunks = append(chunks, strings.TrimSpace(p[:size]))
			p = p[size:]
		}
		if b.Len()+len(p)+2 > size {
			flush()
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p)
	}
	flush()
	return chunks
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
