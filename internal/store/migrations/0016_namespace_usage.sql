-- 0016_namespace_usage — O(1) per-namespace row counters for quota checks (#229).
--
-- checkNodeQuota / checkChunkQuota ran `SELECT count(*) ... WHERE project = $1`
-- on every write: a filtered aggregate whose cost grows with the namespace, even
-- with the project index (0011). Maintain the counts incrementally in a small
-- namespace_usage(project, nodes, chunks) table, updated in-transaction by
-- triggers, so the quota check becomes a single-row primary-key lookup.
--
-- The triggers mirror actual row INSERT/DELETE 1:1, so the counters stay exactly
-- equal to count(*) regardless of which code path (remember, ingest, merge,
-- split, prune, namespace delete) caused the change. A row can also change
-- namespace when its discriminators are re-scoped (disambiguate) — `project` is a
-- STORED generated column of discriminators — so an UPDATE-of-discriminators
-- trigger moves the count between namespaces. Non-discriminator updates
-- (re-embed, tier change, status change) can't move `project`, so those triggers
-- deliberately do not fire on them.

CREATE TABLE namespace_usage (
    project text PRIMARY KEY,
    nodes   bigint NOT NULL DEFAULT 0,
    chunks  bigint NOT NULL DEFAULT 0
);

-- nodes counter: INSERT (+), DELETE (-), and re-scope (net move between projects).
CREATE FUNCTION namespace_usage_nodes() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO namespace_usage (project, nodes)
            SELECT project, count(*)::bigint FROM new_rows GROUP BY project
            ON CONFLICT (project) DO UPDATE SET nodes = namespace_usage.nodes + EXCLUDED.nodes;
    ELSIF TG_OP = 'DELETE' THEN
        INSERT INTO namespace_usage (project, nodes)
            SELECT project, count(*)::bigint FROM old_rows GROUP BY project
            ON CONFLICT (project) DO UPDATE SET nodes = namespace_usage.nodes - EXCLUDED.nodes;
    ELSE -- UPDATE OF discriminators: -1 for each old project, +1 for each new
        INSERT INTO namespace_usage (project, nodes)
            SELECT project, sum(delta)::bigint FROM (
                SELECT project, -1 AS delta FROM old_rows
                UNION ALL
                SELECT project,  1 AS delta FROM new_rows
            ) d GROUP BY project
            ON CONFLICT (project) DO UPDATE SET nodes = namespace_usage.nodes + EXCLUDED.nodes;
    END IF;
    RETURN NULL;
END; $$;

CREATE TRIGGER nodes_usage_ins AFTER INSERT ON nodes
    REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION namespace_usage_nodes();
CREATE TRIGGER nodes_usage_del AFTER DELETE ON nodes
    REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION namespace_usage_nodes();
CREATE TRIGGER nodes_usage_upd AFTER UPDATE OF discriminators ON nodes
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION namespace_usage_nodes();

-- chunks counter: identical shape.
CREATE FUNCTION namespace_usage_chunks() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        INSERT INTO namespace_usage (project, chunks)
            SELECT project, count(*)::bigint FROM new_rows GROUP BY project
            ON CONFLICT (project) DO UPDATE SET chunks = namespace_usage.chunks + EXCLUDED.chunks;
    ELSIF TG_OP = 'DELETE' THEN
        INSERT INTO namespace_usage (project, chunks)
            SELECT project, count(*)::bigint FROM old_rows GROUP BY project
            ON CONFLICT (project) DO UPDATE SET chunks = namespace_usage.chunks - EXCLUDED.chunks;
    ELSE -- UPDATE OF discriminators
        INSERT INTO namespace_usage (project, chunks)
            SELECT project, sum(delta)::bigint FROM (
                SELECT project, -1 AS delta FROM old_rows
                UNION ALL
                SELECT project,  1 AS delta FROM new_rows
            ) d GROUP BY project
            ON CONFLICT (project) DO UPDATE SET chunks = namespace_usage.chunks + EXCLUDED.chunks;
    END IF;
    RETURN NULL;
END; $$;

CREATE TRIGGER chunks_usage_ins AFTER INSERT ON chunks
    REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION namespace_usage_chunks();
CREATE TRIGGER chunks_usage_del AFTER DELETE ON chunks
    REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION namespace_usage_chunks();
CREATE TRIGGER chunks_usage_upd AFTER UPDATE OF discriminators ON chunks
    REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION namespace_usage_chunks();

-- Backfill from the current truth. Migrations are serialized under an advisory
-- lock (#251), so no concurrent writes race this snapshot.
INSERT INTO namespace_usage (project, nodes)
    SELECT project, count(*)::bigint FROM nodes GROUP BY project
    ON CONFLICT (project) DO UPDATE SET nodes = EXCLUDED.nodes;
INSERT INTO namespace_usage (project, chunks)
    SELECT project, count(*)::bigint FROM chunks GROUP BY project
    ON CONFLICT (project) DO UPDATE SET chunks = EXCLUDED.chunks;
