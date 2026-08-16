-- Bounded, redacted keyword windows actually supplied to the second layer.
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS evidence_windows JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE content_moderation_logs
    DROP CONSTRAINT IF EXISTS content_moderation_logs_evidence_windows_array;
ALTER TABLE content_moderation_logs
    ADD CONSTRAINT content_moderation_logs_evidence_windows_array
    CHECK (jsonb_typeof(evidence_windows) = 'array') NOT VALID;
