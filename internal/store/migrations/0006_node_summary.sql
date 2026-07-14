-- 0006_node_summary — nodes gain a human-readable summary text column (#181,
-- Tier 3). Until now a node's description was embedded into summary_embedding and
-- the prose thrown away, so "describe X" had no text to return and the node could
-- not be cited. Store the text alongside the vector; the embedding stays derived
-- from it. Existing rows stay NULL until re-remembered (backfill by re-remember).

ALTER TABLE nodes
    ADD COLUMN summary text;
