-- 0008_valid_time — record WHEN a node/edge stopped being current, so memory can
-- be queried as-of a past date (#200). Until now supersession only flipped status
-- to 'historical' with no timestamp, so "what did we think about X on date Y" was
-- unanswerable. superseded_at is set to now() on the flip to historical (in
-- UpdateNodeStatus/UpdateEdgeStatus) and cleared on a flip back to current.
--
-- Rows already historical before this migration keep NULL (supersession time
-- unknown) and are excluded from as-of reconstruction — only going-forward
-- supersessions carry valid-time.

ALTER TABLE nodes ADD COLUMN superseded_at timestamptz;
ALTER TABLE edges ADD COLUMN superseded_at timestamptz;
