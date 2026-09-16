-- 253_openai_legacy_protection_backfill.sql
-- Extra-merge-only: existing independent OpenAI accounts receive the legacy
-- protection keys. 429 near-limit / auto_pause_* keys are never listed on the
-- right-hand JSON, so PostgreSQL || cannot overwrite them. Shadow, non-OpenAI,
-- random-proxy, and already-protected rows are skipped. Idempotent.

UPDATE accounts
SET extra = (COALESCE(extra, '{}'::jsonb) || jsonb_build_object(
        'anti_degradation', true,
        'protection_scope', 'legacy',
        'codex_fingerprint_mode', 'session',
        'enable_tls_fingerprint', true,
        'tls_fingerprint_builtin', 'nodejs24',
        'anti_degrade', jsonb_build_object(
            'enabled', true,
            'mode', 'legacy',
            'max_concurrency', 0,
            'applied_at', to_char((NOW() AT TIME ZONE 'UTC'), 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
            'prev', jsonb_build_object(
                'codex_fingerprint_mode', extra -> 'codex_fingerprint_mode',
                'enable_tls_fingerprint', extra -> 'enable_tls_fingerprint',
                'tls_fingerprint_builtin', extra -> 'tls_fingerprint_builtin',
                'tls_fingerprint_profile_id', extra -> 'tls_fingerprint_profile_id',
                'proxy_mode', extra -> 'proxy_mode'
            )
        )
    )) - 'tls_fingerprint_profile_id'
WHERE deleted_at IS NULL
  AND platform = 'openai'
  AND parent_account_id IS NULL
  AND COALESCE(extra ->> 'proxy_mode', '') IS DISTINCT FROM 'random'
  AND COALESCE(extra ->> 'anti_degradation', 'false') IS DISTINCT FROM 'true'
  AND COALESCE(extra ->> 'protection_scope', '') NOT IN ('legacy', 'codex_v3')
  AND COALESCE(extra -> 'anti_degrade' ->> 'mode', '') NOT IN ('legacy', 'mode1');
