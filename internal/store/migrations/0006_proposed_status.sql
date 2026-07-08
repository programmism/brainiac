-- 0006_proposed_status — allow the 'proposed' node/edge status for the optional
-- local-LLM extraction review queue (#164). Proposed rows are invisible to every
-- read (all reads filter status='current') until a human approves them (flip to
-- 'current') or rejects them (flip to 'historical').
ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_status_check;
ALTER TABLE nodes ADD CONSTRAINT nodes_status_check
    CHECK (status IN ('current', 'historical', 'proposed'));

ALTER TABLE edges DROP CONSTRAINT IF EXISTS edges_status_check;
ALTER TABLE edges ADD CONSTRAINT edges_status_check
    CHECK (status IN ('current', 'historical', 'proposed'));
