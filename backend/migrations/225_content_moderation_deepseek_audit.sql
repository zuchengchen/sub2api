-- DeepSeek reviewer provenance. Store only structured, redacted outcomes;
-- request credentials and full provider responses are forbidden.
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS deepseek_confidence DECIMAL(6, 5),
    ADD COLUMN IF NOT EXISTS deepseek_category VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS deepseek_reason VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS review_outcome VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reviewer_disagreement BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS review_attempts JSONB NOT NULL DEFAULT '[]'::jsonb;

-- Preserve the absence of a DeepSeek review for historical rows. The ALTERs
-- also repair databases that ran an earlier pre-release draft of this migration.
ALTER TABLE content_moderation_logs
    ALTER COLUMN deepseek_confidence DROP DEFAULT,
    ALTER COLUMN deepseek_confidence DROP NOT NULL;

UPDATE content_moderation_logs
SET deepseek_confidence = NULL
WHERE deepseek_confidence = 0
  AND deepseek_category = ''
  AND deepseek_reason = ''
  AND review_outcome = ''
  AND review_attempts = '[]'::jsonb;

ALTER TABLE content_moderation_logs
    DROP CONSTRAINT IF EXISTS content_moderation_logs_deepseek_confidence_range;
ALTER TABLE content_moderation_logs
    ADD CONSTRAINT content_moderation_logs_deepseek_confidence_range
    CHECK (deepseek_confidence >= 0 AND deepseek_confidence <= 1) NOT VALID;

ALTER TABLE content_moderation_logs
    DROP CONSTRAINT IF EXISTS content_moderation_logs_review_attempts_array;
ALTER TABLE content_moderation_logs
    ADD CONSTRAINT content_moderation_logs_review_attempts_array
    CHECK (jsonb_typeof(review_attempts) = 'array') NOT VALID;

CREATE INDEX IF NOT EXISTS idx_content_moderation_logs_review_outcome
    ON content_moderation_logs(review_outcome, created_at DESC)
    WHERE review_outcome <> '';
