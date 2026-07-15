-- 0010_node_identity_unique — enforce one CURRENT node per identity (#220).
--
-- Node identity is (scope_key, canonical_name). Until now only a plain index
-- backed it, so concurrent remember/link (MCP + WebUI + auto-import interleaving)
-- could read-then-insert and create duplicate current nodes, fragmenting the
-- graph — the #1 named failure mode. A PARTIAL unique index on current rows
-- enforces uniqueness while still allowing multiple 'proposed' extractions and
-- preserved 'historical' rows to share a name (mirrors edges_current_uniq, 0003).
--
-- NOTE: if a database already holds duplicate current nodes (created before this
-- constraint), this index build fails — consolidate/merge them first, then
-- re-run the migration. Fresh installs are unaffected.

CREATE UNIQUE INDEX nodes_identity_current_uniq
    ON nodes (scope_key, canonical_name)
    WHERE status = 'current';
