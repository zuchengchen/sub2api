ALTER TABLE scheduler_dirty_work
    ADD COLUMN IF NOT EXISTS failure_count integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_failure_at timestamptz,
    ADD COLUMN IF NOT EXISTS last_error varchar(512) NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS retry_at timestamptz NOT NULL DEFAULT clock_timestamp();

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'scheduler_dirty_work_failure_count_check'
          AND conrelid = 'scheduler_dirty_work'::regclass
    ) THEN
        ALTER TABLE scheduler_dirty_work
            ADD CONSTRAINT scheduler_dirty_work_failure_count_check
            CHECK (failure_count BETWEEN 0 AND 31);
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS idx_scheduler_dirty_work_retry_pending
    ON scheduler_dirty_work (retry_at, updated_at, kind, entity_id);
