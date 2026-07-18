-- 0020_chunk_sources — multi-source provenance for a chunk (#244, keystone).
--
-- Until now a chunk carried a single source_uri, so identical content ingested
-- from two sources became two rows (chunks_source_hash_uniq is (source_uri,
-- content_hash)), and deleting a source could delete content another source still
-- vouches for. chunk_sources records the *set* of sources a chunk belongs to:
-- membership, keyed (chunk_id, source_uri). This is the schema half of the
-- keystone — it unblocks (a) global content dedup on content_hash and (b)
-- membership-based prune (drop a chunk only when its LAST source is gone), both
-- of which are follow-up #387 so this migration changes no runtime behavior.
--
-- ON DELETE CASCADE: membership is meaningless without its chunk, so a chunk
-- delete (erase, retention, project purge) takes its rows with it — unlike the
-- edges FKs, which are intentionally un-cascaded.

CREATE TABLE chunk_sources (
    chunk_id   uuid NOT NULL REFERENCES chunks (id) ON DELETE CASCADE,
    source_uri text NOT NULL,
    PRIMARY KEY (chunk_id, source_uri)
);

-- Reconcile/prune filters membership by source; index the lookup.
CREATE INDEX chunk_sources_source_uri_idx ON chunk_sources (source_uri);

-- Backfill: every existing chunk belongs to its current source_uri. Idempotent
-- (the PK dedups) so a re-run is a clean no-op.
INSERT INTO chunk_sources (chunk_id, source_uri)
    SELECT id, source_uri FROM chunks
    ON CONFLICT DO NOTHING;
