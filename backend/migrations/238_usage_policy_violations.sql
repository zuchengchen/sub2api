-- Upstream usage-policy violations (OpenAI "Invalid prompt" / usage policy flags).
-- Historical rows are backfilled for stats only; auto_banned stays false.

CREATE TABLE IF NOT EXISTS usage_policy_violations (
    id                   BIGSERIAL PRIMARY KEY,
    user_id              BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_id           VARCHAR(64) NOT NULL DEFAULT '',
    client_request_id    VARCHAR(64) NOT NULL DEFAULT '',
    api_key_id           BIGINT,
    account_id           BIGINT,
    group_id             BIGINT,
    platform             VARCHAR(32) NOT NULL DEFAULT '',
    model                VARCHAR(100) NOT NULL DEFAULT '',
    inbound_endpoint     VARCHAR(256) NOT NULL DEFAULT '',
    status_code          INTEGER,
    upstream_status_code INTEGER,
    error_message        TEXT NOT NULL DEFAULT '',
    auto_banned          BOOLEAN NOT NULL DEFAULT FALSE,
    skip_reason          VARCHAR(32) NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_policy_violations_request_id
    ON usage_policy_violations (request_id)
    WHERE request_id <> '';

CREATE INDEX IF NOT EXISTS idx_usage_policy_violations_user_created
    ON usage_policy_violations (user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_usage_policy_violations_created
    ON usage_policy_violations (created_at DESC);

INSERT INTO usage_policy_violations (
    user_id, request_id, client_request_id, api_key_id, account_id, group_id,
    platform, model, inbound_endpoint, status_code, upstream_status_code,
    error_message, auto_banned, skip_reason, created_at
)
SELECT DISTINCT ON (e.request_id)
    e.user_id,
    e.request_id,
    COALESCE(e.client_request_id, ''),
    e.api_key_id,
    e.account_id,
    e.group_id,
    COALESCE(e.platform, ''),
    COALESCE(NULLIF(e.requested_model, ''), e.model, ''),
    COALESCE(NULLIF(e.inbound_endpoint, ''), e.request_path, ''),
    e.status_code,
    e.upstream_status_code,
    COALESCE(NULLIF(e.upstream_error_message, ''), e.error_message, ''),
    FALSE,
    'backfill',
    e.created_at
FROM ops_error_logs e
WHERE e.user_id IS NOT NULL
  AND e.request_id IS NOT NULL
  AND e.request_id <> ''
  AND EXISTS (SELECT 1 FROM users u WHERE u.id = e.user_id)
  AND (
      e.error_message ILIKE '%flagged as potentially violating our usage policy%'
      OR COALESCE(e.upstream_error_message, '') ILIKE '%flagged as potentially violating our usage policy%'
      OR COALESCE(e.error_body, '') ILIKE '%flagged as potentially violating our usage policy%'
  )
ORDER BY e.request_id, e.id
ON CONFLICT (request_id) WHERE request_id <> '' DO NOTHING;
