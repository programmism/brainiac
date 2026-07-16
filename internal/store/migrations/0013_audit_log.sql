-- 0013_audit_log — an append-only record of who wrote what (#267). A company
-- memory of sensitive data needs to answer "who created/changed/deleted X" for
-- compliance and insider-threat review. Every core write records the acting
-- principal (or 'operator' when unscoped), the operation, the target, and the
-- namespace. Reads are not audited here (higher volume, lower risk) — a follow-up.

CREATE TABLE audit_log (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at         timestamptz NOT NULL DEFAULT now(),
    principal  text NOT NULL,          -- principal name, or 'operator' (Layer 1)
    operation  text NOT NULL,          -- remember | link | supersede | merge | ...
    target     text,                   -- entity name / id / namespace touched
    namespace  text                    -- the project namespace, if scoped
);

CREATE INDEX audit_log_at_idx ON audit_log (at DESC);
