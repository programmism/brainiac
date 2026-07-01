-- 0002_consolidation — support the librarian pass (§11).
-- A persisted staleness flag lets the consolidation pass record findings for
-- human review in the WebUI queue.

ALTER TABLE edges ADD COLUMN flagged_stale boolean NOT NULL DEFAULT false;
CREATE INDEX edges_flagged_stale_idx ON edges (flagged_stale) WHERE flagged_stale;
