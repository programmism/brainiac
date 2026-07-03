-- 0005_chunk_scope — chunks carry the same identity scope as nodes so the
-- retrieval lens (#119) can restrict search to a project + global, not just the
-- graph. Empty = global (default), preserving current cross-project search until
-- a caller passes a project.

ALTER TABLE chunks
    ADD COLUMN discriminators jsonb NOT NULL DEFAULT '{}',
    ADD COLUMN scope_key text NOT NULL DEFAULT '';

CREATE INDEX chunks_scope_key_idx ON chunks (scope_key);
