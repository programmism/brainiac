-- 0004_scoped_identity — a node's identity is its canonical_name PLUS a set of
-- "discriminator" axes (project, env, client, ...). Empty set = global/shared
-- (#117). This lets same-named entities in different projects stay distinct
-- while universal ones (empty discriminators) accrue facts across projects.
--
-- Descriptive/facet tags are NOT identity and live elsewhere; only discriminators
-- participate in the identity key.

ALTER TABLE nodes
    ADD COLUMN discriminators jsonb NOT NULL DEFAULT '{}',
    -- scope_key is the canonical serialization of discriminators used for identity
    -- matching: sorted "k=v" pairs joined by ';'. '' = global. Written by the app
    -- alongside discriminators so the two never drift.
    ADD COLUMN scope_key text NOT NULL DEFAULT '';

-- Existing rows keep discriminators '{}' / scope_key '' → global, preserving
-- current name-only identity behavior.

-- Identity lookups and dedup are keyed by (scope_key, canonical_name).
CREATE INDEX nodes_scope_name_idx ON nodes (scope_key, canonical_name);
