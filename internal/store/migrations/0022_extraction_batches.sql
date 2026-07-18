-- 0022_extraction_batches — track submitted Claude Message Batches for async
-- extraction (#383). #326 shipped the batch client and #383 the separable
-- submit/poll API; this is the job ledger a background poller reads to know which
-- provider batches are outstanding and which have been applied.
--
-- One row per submitted batch: the provider's batch id and a lifecycle status
-- (submitted → ended → applied, or failed). Additive; nothing writes it until the
-- async ingest job is wired (follow-up), so it changes no default behavior.

CREATE TABLE extraction_batches (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_batch_id text NOT NULL,
    status            text NOT NULL DEFAULT 'submitted'
                        CHECK (status IN ('submitted', 'ended', 'applied', 'failed')),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

-- The poller scans by status (submitted/ended), so index it.
CREATE INDEX extraction_batches_status_idx ON extraction_batches (status);
