CREATE TABLE IF NOT EXISTS keypouch_import_jobs (
    job_id VARCHAR(96) PRIMARY KEY,
    email VARCHAR(254) NOT NULL,
    phase TEXT NOT NULL CHECK (phase IN ('pending', 'exchanging', 'failed', 'member')),
    sealed_session TEXT NOT NULL DEFAULT '',
    attempt TEXT NOT NULL DEFAULT '',
    failure TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    account_id BIGINT NOT NULL DEFAULT 0,
    group_id BIGINT NOT NULL,
    CHECK ((phase = 'pending') = (sealed_session <> '')),
    CHECK ((phase = 'exchanging') = (attempt <> '')),
    CHECK ((phase = 'failed') = (failure <> '')),
    CHECK ((phase = 'member') = (account_id > 0))
);
