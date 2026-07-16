-- 0012_chunk_fts — full-text search over chunk text for hybrid retrieval (#211).
--
-- Dense vector search alone reliably misses exact tokens — error codes, ticket
-- IDs, config keys, function names, rare acronyms — because those don't move the
-- embedding much. Add a generated tsvector + GIN index so lexical/BM25-style
-- matching runs alongside vector search; the core fuses the two with RRF.
-- 'english' is the default config; a future per-language path can override it.

ALTER TABLE chunks
    ADD COLUMN tsv tsvector GENERATED ALWAYS AS (to_tsvector('english', text)) STORED;

CREATE INDEX chunks_tsv_idx ON chunks USING gin (tsv);
