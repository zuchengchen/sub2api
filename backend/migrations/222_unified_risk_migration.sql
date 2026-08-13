-- Online preparation and atomic cutover support for retiring Prompt Audit.
-- The staging payload contains metadata and encrypted archive chunks only.
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS legacy_status VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS legacy_event_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS legacy_metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE content_moderation_logs
    ALTER COLUMN user_email TYPE VARCHAR(320),
    ALTER COLUMN api_key_name TYPE VARCHAR(255);

CREATE TABLE IF NOT EXISTS unified_risk_migration_stage (
    source_job_id BIGINT PRIMARY KEY,
    source_updated_at TIMESTAMPTZ NOT NULL,
    source_status VARCHAR(32) NOT NULL,
    source_event_count INTEGER NOT NULL,
    source_fingerprint BYTEA NOT NULL,
    payload_sha256 BYTEA NOT NULL,
    log_payload JSONB NOT NULL,
    archive_payload JSONB,
    staged_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (source_event_count >= 0),
    CHECK (jsonb_typeof(log_payload) = 'object'),
    CHECK (archive_payload IS NULL OR jsonb_typeof(archive_payload) = 'object')
);

CREATE TABLE IF NOT EXISTS unified_risk_migration_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    status VARCHAR(32) NOT NULL,
    source_database VARCHAR(255) NOT NULL,
    source_system_identifier TEXT NOT NULL,
    backup_proof_sha256 BYTEA NOT NULL,
    backup_archive_sha256 BYTEA NOT NULL,
    watermark_job_id BIGINT NOT NULL DEFAULT 0,
    watermark_event_id BIGINT NOT NULL DEFAULT 0,
    staged_job_count BIGINT NOT NULL DEFAULT 0,
    staged_event_count BIGINT NOT NULL DEFAULT 0,
    status_counts JSONB NOT NULL DEFAULT '{}'::jsonb,
    prepared_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(status_counts) = 'object')
);

CREATE TABLE IF NOT EXISTS unified_risk_migration_audits (
    run_id UUID PRIMARY KEY,
    source_database VARCHAR(255) NOT NULL,
    source_system_identifier TEXT NOT NULL,
    backup_proof_sha256 BYTEA NOT NULL,
    backup_archive_sha256 BYTEA NOT NULL,
    job_count BIGINT NOT NULL,
    event_count BIGINT NOT NULL,
    archived_hit_count BIGINT NOT NULL,
    status_counts JSONB NOT NULL,
    finalized_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(status_counts) = 'object')
);
