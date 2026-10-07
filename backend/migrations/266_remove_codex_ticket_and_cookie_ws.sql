-- Remove Codex ticket harvest and Cookie WS extras/settings.

DELETE FROM settings
 WHERE key IN (
     'openai_codex_ticket_enabled',
     'openai_codex_ticket_harvest_proxy_url'
 );

WITH updated_accounts AS (
    UPDATE accounts
    SET extra = extra
            - 'codex_harvest_proxy_url'
    WHERE extra ? 'codex_harvest_proxy_url'
    RETURNING id
)
INSERT INTO scheduler_outbox (event_type, account_id)
SELECT 'account_changed', id
FROM updated_accounts;

UPDATE accounts
SET extra = (
    SELECT COALESCE(jsonb_object_agg(e.key, e.value), '{}'::jsonb)
    FROM jsonb_each(COALESCE(extra, '{}'::jsonb)) AS e(key, value)
    WHERE e.key NOT LIKE 'codex_turn_ticket:%'
      AND e.key NOT LIKE 'codex_turn_ticket_revoked:%'
      AND e.key NOT LIKE 'codex_cookie_ws:%'
)
WHERE extra IS NOT NULL
  AND EXISTS (
      SELECT 1
      FROM jsonb_object_keys(extra) AS k
      WHERE k LIKE 'codex_turn_ticket:%'
         OR k LIKE 'codex_turn_ticket_revoked:%'
         OR k LIKE 'codex_cookie_ws:%'
  );
