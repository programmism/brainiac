-- 0024_extraction_batch_items — per-item context for an async extraction batch
-- (#420). A submitted batch (extraction_batches, #383) carries many chunks by
-- custom_id; when its results come back the poller needs to know, per custom_id,
-- where to attach the extracted nodes/edges (source_uri), under what identity scope
-- (discriminators), and whether to force review (untrusted content, #273). One row
-- per batched chunk; rows cascade away with their batch.
--
-- Additive — written only when the async batch path is used (opt-in), so nothing
-- changes by default.

CREATE TABLE extraction_batch_items (
    batch_id       uuid    NOT NULL REFERENCES extraction_batches (id) ON DELETE CASCADE,
    custom_id      text    NOT NULL,
    source_uri     text    NOT NULL,
    discriminators jsonb   NOT NULL DEFAULT '{}',
    force_review   boolean NOT NULL DEFAULT true,
    PRIMARY KEY (batch_id, custom_id)
);
