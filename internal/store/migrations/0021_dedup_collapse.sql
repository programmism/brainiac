-- 0021_dedup_collapse — enforce global content dedup at the schema level (#393).
--
-- #389 made ingest reuse an existing chunk with the same content within a scope +
-- trust (ChunkIDByHashScoped) instead of storing a duplicate, but rows created
-- BEFORE it — identical content ingested from two sources — still linger, and the
-- old unique index (source_uri, content_hash) still permits new ones. This
-- collapses the legacy duplicates and tightens the index to
-- (content_hash, scope_key, trust) so a chunk's content is stored once per identity
-- scope + trust, with all its sources tracked in chunk_sources.
--
-- DESTRUCTIVE (deletes duplicate rows) but content-preserving: the survivor keeps
-- the text, and every duplicate's chunk_sources membership is repointed onto it
-- FIRST, so no source loses provenance. Idempotent — a re-run finds no duplicates
-- and the index is already relaxed. Runs inside the migration transaction, so it is
-- all-or-nothing. (Operators: run scripts/backup.sh before deploying — cheap habit.)

-- 1. Merge each duplicate group's memberships onto the survivor (smallest id).
WITH survivors AS (
    SELECT content_hash, scope_key, trust, min(id::text)::uuid AS keep_id
    FROM chunks
    WHERE content_hash IS NOT NULL
    GROUP BY content_hash, scope_key, trust
    HAVING count(*) > 1
)
INSERT INTO chunk_sources (chunk_id, source_uri)
    SELECT s.keep_id, cs.source_uri
    FROM chunk_sources cs
    JOIN chunks c ON c.id = cs.chunk_id
    JOIN survivors s
      ON s.content_hash = c.content_hash AND s.scope_key = c.scope_key AND s.trust = c.trust
    WHERE cs.chunk_id <> s.keep_id
    ON CONFLICT DO NOTHING;

-- 2. Delete the duplicate rows (their remaining memberships cascade away).
WITH survivors AS (
    SELECT content_hash, scope_key, trust, min(id::text)::uuid AS keep_id
    FROM chunks
    WHERE content_hash IS NOT NULL
    GROUP BY content_hash, scope_key, trust
    HAVING count(*) > 1
)
DELETE FROM chunks c
    USING survivors s
    WHERE c.content_hash = s.content_hash AND c.scope_key = s.scope_key AND c.trust = s.trust
      AND c.id <> s.keep_id;

-- 3. Tighten the unique index to the dedup key — matches store.ChunkIDByHashScoped
--    and InsertChunk's ON CONFLICT — and drop the old per-source one.
DROP INDEX IF EXISTS chunks_source_hash_uniq;
CREATE UNIQUE INDEX chunks_content_scope_trust_uniq
    ON chunks (content_hash, scope_key, trust)
    WHERE content_hash IS NOT NULL;
