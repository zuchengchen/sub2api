-- Remove the Excel / BPS protocol: drop the default-on trigger, leftover extra
-- keys, persisted BPS Tibo verdicts, and image-relay settings.

DROP TRIGGER IF EXISTS accounts_enforce_openai_excel_bps_extra ON accounts;
DROP FUNCTION IF EXISTS public.enforce_openai_excel_bps_extra();

WITH updated_accounts AS (
    UPDATE accounts
    SET extra = extra
            - 'openai_excel_bps'
            - 'openai_excel_bps_models'
            - 'openai_excel_bps_cache_creation_as_input'
            - 'openai_excel_bps_auto_disable_on_403'
            - 'codex_tibo_verdict:bps'
    WHERE extra ?| ARRAY[
        'openai_excel_bps',
        'openai_excel_bps_models',
        'openai_excel_bps_cache_creation_as_input',
        'openai_excel_bps_auto_disable_on_403',
        'codex_tibo_verdict:bps'
    ]
    RETURNING id
)
INSERT INTO scheduler_outbox (event_type, account_id)
SELECT 'account_changed', id
FROM updated_accounts;

DELETE FROM settings
 WHERE key IN (
     'excel_bps_image_relay_enabled',
     'excel_bps_image_base_url',
     'excel_bps_image_body_limit_mib',
     'excel_bps_image_budget_mib',
     'excel_bps_image_max_requests'
 );
