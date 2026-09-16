-- 254_group_security_policy.sql
-- Group security policy is an extra gate before unified content audit.
-- Group default is off (no-op). Hits are logged as security_policy_* and
-- must never enter auto-ban counting.

ALTER TABLE groups ADD COLUMN IF NOT EXISTS security_policy_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE groups ADD COLUMN IF NOT EXISTS security_policy_mode VARCHAR(20) NOT NULL DEFAULT 'block_session';
ALTER TABLE groups ADD COLUMN IF NOT EXISTS security_policy_email_enabled BOOLEAN NOT NULL DEFAULT TRUE;

CREATE TABLE IF NOT EXISTS security_policy_keywords (
    id BIGSERIAL PRIMARY KEY,
    group_id BIGINT REFERENCES groups(id) ON DELETE CASCADE,
    keyword VARCHAR(200) NOT NULL,
    category VARCHAR(50) NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_secpol_kw_global_unique
    ON security_policy_keywords(keyword)
    WHERE group_id IS NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_secpol_kw_group_unique
    ON security_policy_keywords(group_id, keyword)
    WHERE group_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_secpol_kw_group_id ON security_policy_keywords(group_id);
CREATE INDEX IF NOT EXISTS idx_secpol_kw_enabled ON security_policy_keywords(enabled);
CREATE INDEX IF NOT EXISTS idx_secpol_kw_deleted_at ON security_policy_keywords(deleted_at);

ALTER TABLE content_moderation_logs ADD COLUMN IF NOT EXISTS overturned BOOLEAN NOT NULL DEFAULT FALSE;
