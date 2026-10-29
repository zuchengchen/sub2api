-- Soft-delete leftover spark-shadow accounts and detach them from parents.
-- Historical columns parent_account_id / quota_dimension and migrations 154/154a
-- stay; this feature is retired in application code.
-- Keep account, usage, billing and scheduled-test history intact.

WITH doomed AS (
    SELECT id
    FROM accounts
    WHERE parent_account_id IS NOT NULL
       OR quota_dimension = 'spark'
),
removed_memberships AS (
    DELETE FROM account_groups
    WHERE account_id IN (SELECT id FROM doomed)
    RETURNING account_id, group_id
),
stopped_plans AS (
    UPDATE scheduled_test_plans
    SET enabled = false,
        auto_recover = false,
        next_run_at = NULL,
        updated_at = NOW()
    WHERE (account_id IN (SELECT id FROM doomed)
           OR model_id = 'gpt-5.3-codex-spark')
      AND (enabled OR auto_recover OR next_run_at IS NOT NULL)
),
cleared AS (
    UPDATE accounts
    SET deleted_at = COALESCE(deleted_at, NOW()),
        schedulable = false,
        parent_account_id = NULL,
        quota_dimension = 'global',
        updated_at = NOW()
    WHERE id IN (SELECT id FROM doomed)
    RETURNING id
)
-- A soft-deleted account cannot supply its old groups to the cache consumer.
INSERT INTO scheduler_outbox (event_type, account_id, payload)
SELECT 'account_changed', cleared.id,
       jsonb_build_object('group_ids', COALESCE(
           (SELECT jsonb_agg(group_id ORDER BY group_id)
            FROM removed_memberships
            WHERE account_id = cleared.id), '[]'::jsonb))
FROM cleared;

-- Preserve platform/model ordering and operator fields. Existing empty model
-- lists mean "show every model" and must remain empty. If the retired model was
-- the only selection, keep the existing "other models" bucket instead of
-- accidentally switching that platform to show every real model name.
WITH cleaned_configs AS (
    SELECT config.id,
           jsonb_agg(
               CASE
                   WHEN jsonb_typeof(platform -> 'models') = 'array'
                    AND (platform -> 'models') @> '["gpt-5.3-codex-spark"]'::jsonb
                   THEN jsonb_set(platform, '{models}', (
                       SELECT COALESCE(
                           jsonb_agg(model ORDER BY model_order)
                               FILTER (WHERE model <> '"gpt-5.3-codex-spark"'::jsonb),
                           '["__other__"]'::jsonb)
                       FROM jsonb_array_elements(platform -> 'models')
                           WITH ORDINALITY AS models(model, model_order)
                   ))
                   ELSE platform
               END ORDER BY platform_order
           ) AS platforms
    FROM channel_monitor_v2_config AS config
    CROSS JOIN LATERAL jsonb_array_elements(
        CASE WHEN jsonb_typeof(config.platforms) = 'array'
             THEN config.platforms ELSE '[]'::jsonb END
    ) WITH ORDINALITY AS entries(platform, platform_order)
    GROUP BY config.id
)
UPDATE channel_monitor_v2_config AS config
SET platforms = cleaned.platforms,
    version = config.version + 1,
    updated_at = NOW()
FROM cleaned_configs AS cleaned
WHERE config.id = cleaned.id
  AND config.platforms IS DISTINCT FROM cleaned.platforms;
