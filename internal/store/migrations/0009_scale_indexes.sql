-- 0009_scale_indexes — indexes for hot paths that seq-scanned at scale (#227, #228).
--
-- (1) nodes.summary_embedding had NO vector index despite being queried by every
--     remember (dedup) and recall (FindSimilarNodes ORDER BY <=>). Add an HNSW over
--     the current tier only, mirroring the chunk hot-index, so node semantic dedup
--     and recall stop scanning every in-scope node.
-- (2) chunks/edges were filtered by source_uri (re-ingest reconcile, staleness join,
--     evidence lookup) with only content_hash indexed, so per-document work grew with
--     the whole corpus. Index source_uri.

CREATE INDEX nodes_summary_embedding_idx
    ON nodes USING hnsw (summary_embedding halfvec_cosine_ops)
    WHERE status = 'current';

CREATE INDEX chunks_source_uri_idx ON chunks (source_uri);

CREATE INDEX edges_source_uri_current_idx ON edges (source_uri) WHERE status = 'current';
