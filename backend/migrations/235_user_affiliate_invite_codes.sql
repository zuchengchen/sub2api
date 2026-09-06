-- One-time affiliate invite codes.
-- Each code can bind exactly one invitee. The affiliate page always shows an
-- unused code and mints a new one after the current code is consumed.
CREATE TABLE IF NOT EXISTS user_affiliate_invite_codes (
    id BIGSERIAL PRIMARY KEY,
    inviter_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code VARCHAR(32) NOT NULL UNIQUE,
    used_by BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    used_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_affiliate_invite_codes_used_by
    ON user_affiliate_invite_codes (used_by)
    WHERE used_by IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_user_affiliate_invite_codes_inviter_unused
    ON user_affiliate_invite_codes (inviter_id, id DESC)
    WHERE used_by IS NULL;

COMMENT ON TABLE user_affiliate_invite_codes IS '一次性邀请码：每个码只能给一名用户注册';
COMMENT ON COLUMN user_affiliate_invite_codes.code IS '一次性邀请码';
COMMENT ON COLUMN user_affiliate_invite_codes.used_by IS '使用该码完成注册的用户 ID';

-- Seed existing identity aff_codes as the first unused one-time token so
-- already-shared /register?aff= links keep working once.
INSERT INTO user_affiliate_invite_codes (inviter_id, code, created_at)
SELECT ua.user_id, ua.aff_code, ua.created_at
FROM user_affiliates ua
ON CONFLICT (code) DO NOTHING;
