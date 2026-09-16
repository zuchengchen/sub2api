-- The singleton epoch is advanced only by the session that already holds the
-- scheduler advisory lock. Owner-gated writes compare against it to fence a
-- previous session after takeover.
CREATE TABLE IF NOT EXISTS scheduler_ownership_epoch (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    epoch bigint NOT NULL CHECK (epoch > 0)
);

REVOKE ALL ON scheduler_ownership_epoch FROM PUBLIC;
