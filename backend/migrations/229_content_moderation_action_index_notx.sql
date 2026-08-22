-- The review-failure view filters by action and orders by newest first. Keep
-- this index concurrent so production reads and writes are not blocked while
-- upgrading an existing audit table.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_content_moderation_logs_action_created_at
    ON content_moderation_logs(action, created_at DESC);
