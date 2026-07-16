-- 0014_chunk_source_hash_uniq — one chunk per (source_uri, content_hash) (#225).
--
-- Ingest reads existing hashes outside the reconcile transaction, so two
-- concurrent ingests of the same source could double-write identical chunks. A
-- partial unique index (only where a hash is present) makes InsertChunk an
-- idempotent upsert and closes that race, without constraining hash-less chunks.

CREATE UNIQUE INDEX chunks_source_hash_uniq
    ON chunks (source_uri, content_hash)
    WHERE content_hash IS NOT NULL;
