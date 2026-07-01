-- 0003_edge_uniqueness — one current edge per (from, to, type).
-- Prevents duplicate edges from repeated link/import/extractor runs (§11).

-- Collapse any pre-existing duplicate current edges, keeping the newest.
UPDATE edges SET status = 'historical'
WHERE status = 'current' AND id NOT IN (
    SELECT DISTINCT ON (from_id, to_id, type) id
    FROM edges
    WHERE status = 'current'
    ORDER BY from_id, to_id, type, created_at DESC
);

CREATE UNIQUE INDEX edges_current_uniq
    ON edges (from_id, to_id, type)
    WHERE status = 'current';
