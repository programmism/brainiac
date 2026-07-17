-- 0015_source_sync — persisted per-source incremental-sync state (#236). Auto-import
-- re-scans every file each interval; with a stored last-synced modification time
-- per source_uri, an incremental run can skip a document whose source hasn't
-- advanced since it was last ingested, instead of re-chunking + re-hashing it.
-- One row per document (source_uri); the connector-agnostic scheme prefix
-- (e.g. markdown://) keeps sources from colliding.

CREATE TABLE source_sync (
    source_uri  text PRIMARY KEY,
    modified_at timestamptz,            -- source's last-modified time at last sync
    synced_at   timestamptz NOT NULL DEFAULT now()
);
