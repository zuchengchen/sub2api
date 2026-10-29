-- Retire scheduled account tests while retaining plans and results as history.
-- Only changed plans get a new timestamp, so reruns leave history untouched.
UPDATE scheduled_test_plans
SET enabled = false,
    auto_recover = false,
    next_run_at = NULL,
    updated_at = NOW()
WHERE enabled OR auto_recover OR next_run_at IS NOT NULL;
