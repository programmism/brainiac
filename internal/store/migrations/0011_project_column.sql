-- 0011_project_column — make the Layer-2 isolation wall sargable (#226).
--
-- The wall/quota/export/delete predicate was
--   COALESCE(NULLIF(discriminators->>'project',''),'') = ANY($n)
-- a non-indexable expression re-evaluated on every walled read, quota count,
-- export, and namespace-delete — a full scan at scale. Materialize it as a STORED
-- generated column and btree-index it, so all those paths become index lookups.
-- The generated column stays in sync with discriminators automatically (Postgres
-- 12+), so nothing has to maintain it.

ALTER TABLE nodes
    ADD COLUMN project text GENERATED ALWAYS AS (COALESCE(NULLIF(discriminators->>'project',''),'')) STORED;
CREATE INDEX nodes_project_idx ON nodes (project);

ALTER TABLE chunks
    ADD COLUMN project text GENERATED ALWAYS AS (COALESCE(NULLIF(discriminators->>'project',''),'')) STORED;
CREATE INDEX chunks_project_idx ON chunks (project);
