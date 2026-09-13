-- Backfill wind-control audit rows for historical usage-policy hits.
-- Conversation bodies are not available for these older ops errors.

INSERT INTO content_moderation_logs (
    request_id, user_id, user_email, api_key_id, api_key_name, group_id, group_name,
    endpoint, provider, model, mode, action, flagged, highest_category, highest_score,
    error, archive_status, created_at
)
SELECT
    v.request_id,
    v.user_id,
    COALESCE(u.email, ''),
    CASE WHEN k.id IS NULL THEN NULL ELSE v.api_key_id END,
    COALESCE(k.name, ''),
    CASE WHEN g.id IS NULL THEN NULL ELSE v.group_id END,
    COALESCE(g.name, ''),
    COALESCE(NULLIF(v.inbound_endpoint, ''), '/v1/responses'),
    COALESCE(NULLIF(v.platform, ''), 'openai'),
    COALESCE(v.model, ''),
    'post_upstream',
    'usage_policy',
    TRUE,
    'usage_policy',
    1.0,
    COALESCE(v.error_message, ''),
    'none',
    v.created_at
FROM usage_policy_violations v
LEFT JOIN users u ON u.id = v.user_id
LEFT JOIN api_keys k ON k.id = v.api_key_id
LEFT JOIN groups g ON g.id = v.group_id
WHERE v.request_id <> ''
  AND NOT EXISTS (
      SELECT 1
      FROM content_moderation_logs l
      WHERE l.action = 'usage_policy'
        AND l.request_id = v.request_id
  );
