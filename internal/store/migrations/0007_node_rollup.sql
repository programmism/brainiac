-- 0007_node_rollup — nodes gain a "current state of X" rollup text (#198). A hub
-- node with many edges accumulates a long detailed history; a rollup is a curated
-- synthesis of its *current* state over that history (SYSTEM.md §8). Distinct from
-- `summary` (the node's identity description): rollup is descriptive prose, never
-- identity, so it never affects dedup. NULL until a rollup is written.

ALTER TABLE nodes
    ADD COLUMN rollup text;
