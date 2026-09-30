CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_route_degraded_created_at
    ON usage_logs (created_at DESC, id DESC)
    WHERE route_degraded IS TRUE;
