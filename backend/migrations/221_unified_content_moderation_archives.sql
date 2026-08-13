-- Unified Risk Control archive metadata. Ciphertext is deliberately isolated
-- from the list table so ordinary administration queries cannot load it.
ALTER TABLE content_moderation_logs
    ADD COLUMN IF NOT EXISTS protocol VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS transport VARCHAR(32) NOT NULL DEFAULT 'http',
    ADD COLUMN IF NOT EXISTS request_stage VARCHAR(64) NOT NULL DEFAULT 'http',
    ADD COLUMN IF NOT EXISTS request_target TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS input_hash VARCHAR(64) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS archive_id UUID,
    ADD COLUMN IF NOT EXISTS archive_version INTEGER,
    ADD COLUMN IF NOT EXISTS archive_key_id VARCHAR(128) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS archive_plaintext_sha256 BYTEA,
    ADD COLUMN IF NOT EXISTS archive_plaintext_bytes BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS archive_status VARCHAR(32) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS archive_incomplete BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS archive_content_lost BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS archive_deleted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS disposition_status VARCHAR(32) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS disposition_target VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS disposition_transitioned BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS email_delivery_status VARCHAR(16) NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS email_delivery_claimed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS legacy_source_job_id BIGINT;

UPDATE content_moderation_logs
SET email_delivery_status = 'sent',
    email_delivery_claimed_at = COALESCE(email_delivery_claimed_at, created_at, NOW())
WHERE email_sent = TRUE AND email_delivery_claimed_at IS NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'content_moderation_logs'::regclass
          AND conname = 'content_moderation_logs_email_delivery_status_check'
    ) THEN
        ALTER TABLE content_moderation_logs
            ADD CONSTRAINT content_moderation_logs_email_delivery_status_check
            CHECK (email_delivery_status IN ('none', 'claimed', 'sent', 'failed'));
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS idx_content_moderation_logs_archive_id
    ON content_moderation_logs(archive_id) WHERE archive_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_content_moderation_logs_legacy_source_job
    ON content_moderation_logs(legacy_source_job_id) WHERE legacy_source_job_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_content_moderation_logs_archive_status
    ON content_moderation_logs(archive_status, created_at DESC)
    WHERE archive_status <> 'none';

CREATE TABLE IF NOT EXISTS content_moderation_log_chunks (
    log_id BIGINT NOT NULL REFERENCES content_moderation_logs(id) ON DELETE CASCADE,
    archive_id UUID NOT NULL,
    chunk_index INTEGER NOT NULL,
    chunk_total INTEGER NOT NULL,
    nonce BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    plaintext_bytes INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (log_id, chunk_index),
    UNIQUE (archive_id, chunk_index),
    CHECK (chunk_index >= 0),
    CHECK (chunk_total > 0),
    CHECK (chunk_index < chunk_total),
    CHECK (octet_length(nonce) = 24),
    CHECK (plaintext_bytes >= 0)
);

CREATE INDEX IF NOT EXISTS idx_content_moderation_log_chunks_archive
    ON content_moderation_log_chunks(archive_id, chunk_index);

-- Sensitive archive operations are append-only and intentionally separate
-- from the general audit-log retention policy.
CREATE TABLE IF NOT EXISTS content_moderation_archive_access_audits (
    id BIGSERIAL PRIMARY KEY,
    log_id BIGINT NOT NULL REFERENCES content_moderation_logs(id),
    archive_id UUID,
    actor_user_id BIGINT REFERENCES users(id),
    action VARCHAR(32) NOT NULL,
    request_id VARCHAR(255) NOT NULL DEFAULT '',
    result VARCHAR(32) NOT NULL,
    bytes_served BIGINT NOT NULL DEFAULT 0,
    detail TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_content_moderation_archive_audits_log
    ON content_moderation_archive_access_audits(log_id, created_at DESC);

-- Retry state is idempotent by operation key and retained until success. A
-- worker may lease a row; capped exponential backoff changes next_attempt_at
-- but never drops the operation after a finite number of attempts.
CREATE TABLE IF NOT EXISTS content_moderation_retry_operations (
    id BIGSERIAL PRIMARY KEY,
    operation_key VARCHAR(255) NOT NULL UNIQUE,
    operation_type VARCHAR(32) NOT NULL,
    log_id BIGINT REFERENCES content_moderation_logs(id),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    leased_until TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_content_moderation_retry_due
    ON content_moderation_retry_operations(next_attempt_at, id)
    WHERE completed_at IS NULL;
