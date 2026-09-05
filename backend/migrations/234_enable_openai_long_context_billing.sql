-- Sync OpenAI long-context billing with API rates by default:
-- prompt (input + cache) over 272K uses 2x input/cache and 1.5x output.
-- Replaces the opt-in default introduced in 175.

CREATE OR REPLACE FUNCTION public.enforce_openai_long_context_billing_extra()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    parent_effective_value JSONB;
BEGIN
    IF NEW.platform IS DISTINCT FROM 'openai' THEN
        RETURN NEW;
    END IF;

    NEW.extra := COALESCE(NEW.extra, '{}'::jsonb);
    IF NEW.parent_account_id IS NOT NULL AND NEW.quota_dimension = 'spark' THEN
        SELECT CASE
            WHEN parent.platform IS DISTINCT FROM 'openai' THEN 'true'::jsonb
            WHEN NOT (COALESCE(parent.extra, '{}'::jsonb) ? 'openai_long_context_billing_enabled') THEN 'true'::jsonb
            WHEN jsonb_typeof(parent.extra->'openai_long_context_billing_enabled') = 'boolean'
                THEN parent.extra->'openai_long_context_billing_enabled'
            ELSE 'true'::jsonb
        END
        INTO parent_effective_value
        FROM accounts AS parent
        WHERE parent.id = NEW.parent_account_id;

        NEW.extra := jsonb_set(
            NEW.extra,
            '{openai_long_context_billing_enabled}',
            COALESCE(parent_effective_value, 'true'::jsonb),
            true
        );
    ELSIF NOT (NEW.extra ? 'openai_long_context_billing_enabled')
        AND TG_OP = 'UPDATE'
        AND OLD.platform = 'openai'
        AND jsonb_typeof(OLD.extra->'openai_long_context_billing_enabled') = 'boolean' THEN
        NEW.extra := jsonb_set(
            NEW.extra,
            '{openai_long_context_billing_enabled}',
            OLD.extra->'openai_long_context_billing_enabled',
            true
        );
    ELSIF NOT (NEW.extra ? 'openai_long_context_billing_enabled') THEN
        NEW.extra := jsonb_set(
            NEW.extra,
            '{openai_long_context_billing_enabled}',
            'true'::jsonb,
            true
        );
    END IF;

    IF jsonb_typeof(NEW.extra->'openai_long_context_billing_enabled') IS DISTINCT FROM 'boolean' THEN
        RAISE EXCEPTION 'openai_long_context_billing_enabled must be a boolean'
            USING ERRCODE = '22023';
    END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION public.propagate_openai_long_context_billing_extra_to_shadows()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    WITH updated_shadows AS (
        UPDATE accounts AS shadow
        SET extra = jsonb_set(
            COALESCE(shadow.extra, '{}'::jsonb),
            '{openai_long_context_billing_enabled}',
            NEW.extra->'openai_long_context_billing_enabled',
            true
        )
        WHERE shadow.parent_account_id = NEW.id
          AND shadow.platform = 'openai'
          AND shadow.quota_dimension = 'spark'
          AND shadow.extra->'openai_long_context_billing_enabled'
              IS DISTINCT FROM NEW.extra->'openai_long_context_billing_enabled'
        RETURNING shadow.id
    )
    INSERT INTO scheduler_outbox (event_type, account_id)
    SELECT 'account_changed', id
    FROM updated_shadows;
    RETURN NULL;
END;
$$;

WITH updated_parents AS (
    UPDATE accounts
    SET extra = jsonb_set(
        COALESCE(extra, '{}'::jsonb),
        '{openai_long_context_billing_enabled}',
        'true'::jsonb,
        true
    )
    WHERE platform = 'openai'
      AND parent_account_id IS NULL
      AND COALESCE((extra->>'openai_long_context_billing_enabled')::boolean, false) IS DISTINCT FROM TRUE
    RETURNING id
)
INSERT INTO scheduler_outbox (event_type, account_id)
SELECT 'account_changed', id
FROM updated_parents;
