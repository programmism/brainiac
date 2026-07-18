-- 0019_current_partial_indexes — keep the hot working set small as history grows
-- (#230, safe slice of table partitioning).
--
-- Supersession never deletes: an updated node/edge flips status 'current' ->
-- 'historical' and stays. So edges_from_idx/edges_to_idx (full, from 0001) and
-- nodes_project_idx (full, from 0011) index every historical and proposed row too,
-- and grow without bound while almost every read wants only the current tier
-- (graph traversal `(from_id=$1 OR to_id=$1) AND status='current'`, consolidation's
-- `from_id=$1 AND status='current'`, the current-node scan in ListGraph). A real
-- table partition would split current/historical physically but needs a table
-- rewrite — too risky to auto-apply at boot. Partial indexes get most of the win
-- with none of the risk: they cover only current rows, so the hot index stays the
-- size of live memory regardless of how much history piles up. Additive — the full
-- indexes remain for all-status paths (erase, as-of over historical). The planner
-- picks the smaller partial index for current-filtered queries.
--
-- Plain CREATE INDEX (not CONCURRENTLY): migrations run inside a BEGIN/COMMIT
-- (applyOne), where CONCURRENTLY is illegal. These briefly lock the table, which is
-- fine for an expand migration applied at boot on a fresh or small database.

CREATE INDEX edges_from_current_idx ON edges (from_id) WHERE status = 'current';
CREATE INDEX edges_to_current_idx   ON edges (to_id)   WHERE status = 'current';
CREATE INDEX nodes_project_current_idx ON nodes (project) WHERE status = 'current';

-- superseded_at powers as-of reconstruction (#200) and the retention sweep (#363,
-- `superseded_at < cutoff`); only historical rows carry it, so a partial index on
-- NOT NULL stays small and skips the whole current tier.
CREATE INDEX nodes_superseded_at_idx ON nodes (superseded_at) WHERE superseded_at IS NOT NULL;
CREATE INDEX edges_superseded_at_idx ON edges (superseded_at) WHERE superseded_at IS NOT NULL;
