-- YuFeng XGuard policy provenance and fragment-cache replay linkage.
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS cache_hit BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS decision_source VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_log_id BIGINT,
    ADD COLUMN IF NOT EXISTS replay_of_input_hash VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS fragment_role VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS fragment_kind VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS context_class VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS fragment_path VARCHAR(512) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS cache_namespace VARCHAR(192) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS policy_version VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS model_profile VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS prompt_version VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS evidence_policy_version VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS keyword_tier VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS keyword_rule_id VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS evidence_mode VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS evidence_truncated BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS parser_status VARCHAR(32) NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_content_moderation_logs_source_log_id
    ON content_moderation_logs(source_log_id) WHERE source_log_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_content_moderation_logs_replay_hash
    ON content_moderation_logs(replay_of_input_hash) WHERE replay_of_input_hash <> '';
CREATE INDEX IF NOT EXISTS idx_content_moderation_logs_context_profile
    ON content_moderation_logs(context_class, model_profile, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_content_moderation_logs_decision_source
    ON content_moderation_logs(decision_source, created_at DESC);
