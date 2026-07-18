-- 0017_chunk_trust — provenance/trust tag on chunks (#273).
--
-- Ingested document text is untrusted input: the optional extractor consumes it,
-- and recalled chunk text reaches downstream agents — both indirect prompt-
-- injection vectors. Tag each chunk's trust so extraction from untrusted content
-- is forced through review (never auto-written live) and clients can treat
-- recalled text as data, not instructions.
--
-- Expand-only (#261): NOT NULL DEFAULT 'trusted' grandfathers every existing row,
-- so an older binary keeps working and this release is backward-compatible.
ALTER TABLE chunks
    ADD COLUMN trust text NOT NULL DEFAULT 'trusted'
    CHECK (trust IN ('trusted', 'untrusted'));
