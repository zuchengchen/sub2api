-- Timed usage-policy user bans. Auto-unban worker selects rows with
-- usage_policy_unban_at <= NOW(); admin re-enable should clear the timestamp.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS usage_policy_unban_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_users_usage_policy_unban_at
    ON users (usage_policy_unban_at)
    WHERE deleted_at IS NULL
      AND usage_policy_unban_at IS NOT NULL;
