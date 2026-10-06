-- Default Excel / BPS on for regular ChatGPT OAuth accounts.
-- Missing extra is treated as enabled in application code; this backfill
-- writes the flag so admin UI and bulk edits see the same default.

CREATE OR REPLACE FUNCTION public.enforce_openai_excel_bps_extra()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    auth_mode TEXT;
BEGIN
    IF NEW.platform IS DISTINCT FROM 'openai' OR NEW.type IS DISTINCT FROM 'oauth' THEN
        RETURN NEW;
    END IF;
    IF NEW.parent_account_id IS NOT NULL THEN
        RETURN NEW;
    END IF;

    auth_mode := lower(btrim(COALESCE(
        NEW.credentials ->> 'auth_mode',
        NEW.credentials ->> 'openai_auth_mode',
        ''
    )));
    IF auth_mode IN ('agentidentity', 'personalaccesstoken', 'personal_access_token') THEN
        RETURN NEW;
    END IF;

    NEW.extra := COALESCE(NEW.extra, '{}'::jsonb);
    IF NOT (NEW.extra ? 'openai_excel_bps')
        AND TG_OP = 'UPDATE'
        AND OLD.platform = 'openai'
        AND OLD.type = 'oauth'
        AND jsonb_typeof(OLD.extra -> 'openai_excel_bps') = 'boolean' THEN
        NEW.extra := jsonb_set(
            NEW.extra,
            '{openai_excel_bps}',
            OLD.extra -> 'openai_excel_bps',
            true
        );
    ELSIF NOT (NEW.extra ? 'openai_excel_bps') THEN
        NEW.extra := jsonb_set(
            NEW.extra,
            '{openai_excel_bps}',
            'true'::jsonb,
            true
        );
    END IF;

    IF jsonb_typeof(NEW.extra -> 'openai_excel_bps') IS DISTINCT FROM 'boolean' THEN
        RAISE EXCEPTION 'openai_excel_bps must be a boolean'
            USING ERRCODE = '22023';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS accounts_enforce_openai_excel_bps_extra ON accounts;
CREATE TRIGGER accounts_enforce_openai_excel_bps_extra
BEFORE INSERT OR UPDATE OF platform, type, extra, credentials, parent_account_id
ON accounts
FOR EACH ROW
EXECUTE FUNCTION public.enforce_openai_excel_bps_extra();

WITH updated_accounts AS (
    UPDATE accounts
    SET extra = jsonb_set(
        COALESCE(extra, '{}'::jsonb),
        '{openai_excel_bps}',
        'true'::jsonb,
        true
    )
    WHERE deleted_at IS NULL
      AND platform = 'openai'
      AND type = 'oauth'
      AND parent_account_id IS NULL
      AND lower(btrim(COALESCE(
            credentials ->> 'auth_mode',
            credentials ->> 'openai_auth_mode',
            ''
          ))) NOT IN ('agentidentity', 'personalaccesstoken', 'personal_access_token')
      AND COALESCE((extra ->> 'openai_excel_bps')::boolean, false) IS DISTINCT FROM TRUE
    RETURNING id
)
INSERT INTO scheduler_outbox (event_type, account_id)
SELECT 'account_changed', id
FROM updated_accounts;
