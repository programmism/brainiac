-- 0001_init — Brainiac core schema (SYSTEM.md §5, PRD Appendix A).
-- Domain-neutral: two layers (vector chunks + curated graph) in one Postgres.
-- Applied inside a transaction by the runner; do not add transaction control here.

CREATE EXTENSION IF NOT EXISTS vector;

-- Layer 1 — semantic search over curated chunks.
CREATE TABLE chunks (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    text               text NOT NULL,                       -- raw chunk text, always stored (re-embed without re-reading sources)
    embedding          halfvec(768),                        -- quantized vector; 768 = nomic-embed-text dims
    source_uri         text NOT NULL,
    source_locator     jsonb,                               -- position/line range/heading — provenance
    quality_score      real,
    tier               text NOT NULL DEFAULT 'hot' CHECK (tier IN ('hot', 'cold')),
    content_hash       text,                                -- dedup + change detection
    created_at         timestamptz NOT NULL DEFAULT now(),
    source_modified_at timestamptz
);

-- HNSW cosine index over the hot tier only; cold is excluded from default search.
CREATE INDEX chunks_embedding_hot_idx
    ON chunks USING hnsw (embedding halfvec_cosine_ops)
    WHERE tier = 'hot';
CREATE INDEX chunks_content_hash_idx ON chunks (content_hash);

-- Layer 2 — curated graph: nodes.
CREATE TABLE nodes (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    canonical_name    text NOT NULL,
    aliases           text[] NOT NULL DEFAULT '{}',
    type              text,                                 -- service/datastore/decision/... (domain-defined)
    summary_embedding halfvec(768),                         -- for semantic node dedup
    status            text NOT NULL DEFAULT 'current' CHECK (status IN ('current', 'historical')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    last_confirmed_at timestamptz
);
CREATE INDEX nodes_canonical_name_idx ON nodes (canonical_name);

-- Layer 2 — curated graph: edges. Every edge carries why + provenance + author.
CREATE TABLE edges (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    from_id           uuid NOT NULL REFERENCES nodes (id),
    to_id             uuid NOT NULL REFERENCES nodes (id),
    type              text NOT NULL,                        -- writes_to/depends_on/rejected/supersedes/...
    why               text,                                 -- the rationale — core value field
    source_uri        text,
    source_locator    jsonb,
    author            text,
    status            text NOT NULL DEFAULT 'current' CHECK (status IN ('current', 'historical')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    last_confirmed_at timestamptz
);
CREATE INDEX edges_from_idx ON edges (from_id);
CREATE INDEX edges_to_idx ON edges (to_id);
