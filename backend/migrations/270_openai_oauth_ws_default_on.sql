-- OpenAI OAuth/SetupToken accounts now default to the Responses WebSocket v2
-- upstream, also for HTTP clients. Existing accounts carry an explicit off
-- written by the old create-dialog default, so flip only those values.
-- API-key accounts, openai_ws_force_http and every other extra key stay as-is.
-- Idempotent: a rerun finds no remaining off/false values.
UPDATE accounts
SET extra = jsonb_set(extra, '{openai_oauth_responses_websockets_v2_mode}', '"ctx_pool"'::jsonb, false)
WHERE deleted_at IS NULL
  AND platform = 'openai'
  AND type IN ('oauth', 'setup-token')
  AND extra->>'openai_oauth_responses_websockets_v2_mode' = 'off';

UPDATE accounts
SET extra = jsonb_set(extra, '{openai_oauth_responses_websockets_v2_enabled}', 'true'::jsonb, false)
WHERE deleted_at IS NULL
  AND platform = 'openai'
  AND type IN ('oauth', 'setup-token')
  AND extra->'openai_oauth_responses_websockets_v2_enabled' = 'false'::jsonb;
