-- Canonical scheduler work. Only the elected scheduler owner promotes rows
-- into this table; business-table triggers write source-local evidence below.
CREATE TABLE IF NOT EXISTS scheduler_dirty_work (
    kind smallint NOT NULL,
    entity_id bigint NOT NULL,
    generation bigint NOT NULL DEFAULT 1,
    rebuild_buckets boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT scheduler_dirty_work_pkey PRIMARY KEY (kind, entity_id),
    CONSTRAINT scheduler_dirty_work_kind_check CHECK (kind IN (1, 2, 3)),
    CONSTRAINT scheduler_dirty_work_entity_check CHECK (
        (kind = 1 AND entity_id > 0) OR
        (kind = 2 AND entity_id >= 0) OR
        (kind = 3 AND entity_id = 0)
    ),
    CONSTRAINT scheduler_dirty_work_generation_check CHECK (generation > 0)
);

CREATE INDEX IF NOT EXISTS idx_scheduler_dirty_work_pending
    ON scheduler_dirty_work (updated_at, kind, entity_id);

-- Source cardinality is bounded by distinct unpromoted domain identities.
-- Account bucket_dirty is sticky until the exact source generation is promoted.
CREATE TABLE IF NOT EXISTS scheduler_dirty_account_sources (
    account_id bigint PRIMARY KEY,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    bucket_dirty boolean NOT NULL DEFAULT false,
    group_cursor bigint NOT NULL DEFAULT 0 CHECK (group_cursor >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS idx_scheduler_dirty_account_sources_pending
    ON scheduler_dirty_account_sources (updated_at, account_id);

CREATE TABLE IF NOT EXISTS scheduler_dirty_group_sources (
    group_id bigint PRIMARY KEY,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX IF NOT EXISTS idx_scheduler_dirty_group_sources_pending
    ON scheduler_dirty_group_sources (updated_at, group_id);

-- Deliberately has no foreign keys. A physical membership delete must leave
-- pair-keyed evidence after either parent row and its memberships are gone.
CREATE TABLE IF NOT EXISTS scheduler_dirty_membership_sources (
    account_id bigint NOT NULL,
    group_id bigint NOT NULL,
    generation bigint NOT NULL DEFAULT 1 CHECK (generation > 0),
    group_cursor bigint NOT NULL DEFAULT 0 CHECK (group_cursor >= 0),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, group_id)
);

CREATE INDEX IF NOT EXISTS idx_scheduler_dirty_membership_sources_pending
    ON scheduler_dirty_membership_sources (updated_at, account_id, group_id);

-- Transition-table producers coalesce each statement in primary-key order.
-- They never touch canonical work, group zero, global rows, or another source
-- identity type, avoiding business-row/canonical lock-order cycles.
CREATE OR REPLACE FUNCTION scheduler_accounts_source_from_insert()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_account_sources (account_id, bucket_dirty)
    SELECT id, true FROM new_rows ORDER BY id
    ON CONFLICT (account_id) DO UPDATE
    SET generation = public.scheduler_dirty_account_sources.generation + 1,
        bucket_dirty = true,
        group_cursor = 0,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_accounts_source_from_update()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_account_sources (account_id, bucket_dirty)
    SELECT
        COALESCE(n.id, o.id),
        CASE
            WHEN n.id IS NULL OR o.id IS NULL THEN true
            ELSE
                o.platform IS DISTINCT FROM n.platform OR
                o.priority IS DISTINCT FROM n.priority OR
                o.status IS DISTINCT FROM n.status OR
                o.expires_at IS DISTINCT FROM n.expires_at OR
                o.schedulable IS DISTINCT FROM n.schedulable OR
                o.rate_limit_reset_at IS DISTINCT FROM n.rate_limit_reset_at OR
                o.overload_until IS DISTINCT FROM n.overload_until OR
                o.temp_unschedulable_until IS DISTINCT FROM n.temp_unschedulable_until OR
                o.deleted_at IS DISTINCT FROM n.deleted_at OR
                o.extra -> 'mixed_scheduling' IS DISTINCT FROM n.extra -> 'mixed_scheduling'
        END
    FROM new_rows AS n
    FULL JOIN old_rows AS o USING (id)
    WHERE
        n.id IS NULL OR
        o.id IS NULL OR
        o.name IS DISTINCT FROM n.name OR
        o.platform IS DISTINCT FROM n.platform OR
        o.type IS DISTINCT FROM n.type OR
        o.credentials IS DISTINCT FROM n.credentials OR
        o.extra IS DISTINCT FROM n.extra OR
        o.proxy_id IS DISTINCT FROM n.proxy_id OR
        o.concurrency IS DISTINCT FROM n.concurrency OR
        o.load_factor IS DISTINCT FROM n.load_factor OR
        o.priority IS DISTINCT FROM n.priority OR
        o.rate_multiplier IS DISTINCT FROM n.rate_multiplier OR
        o.status IS DISTINCT FROM n.status OR
        o.expires_at IS DISTINCT FROM n.expires_at OR
        o.auto_pause_on_expired IS DISTINCT FROM n.auto_pause_on_expired OR
        o.schedulable IS DISTINCT FROM n.schedulable OR
        o.rate_limited_at IS DISTINCT FROM n.rate_limited_at OR
        o.rate_limit_reset_at IS DISTINCT FROM n.rate_limit_reset_at OR
        o.overload_until IS DISTINCT FROM n.overload_until OR
        o.temp_unschedulable_until IS DISTINCT FROM n.temp_unschedulable_until OR
        o.temp_unschedulable_reason IS DISTINCT FROM n.temp_unschedulable_reason OR
        o.session_window_start IS DISTINCT FROM n.session_window_start OR
        o.session_window_end IS DISTINCT FROM n.session_window_end OR
        o.session_window_status IS DISTINCT FROM n.session_window_status OR
        o.deleted_at IS DISTINCT FROM n.deleted_at
    ORDER BY COALESCE(n.id, o.id)
    ON CONFLICT (account_id) DO UPDATE
    SET generation = public.scheduler_dirty_account_sources.generation + 1,
        bucket_dirty = public.scheduler_dirty_account_sources.bucket_dirty OR EXCLUDED.bucket_dirty,
        group_cursor = 0,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_accounts_source_from_old()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_account_sources (account_id, bucket_dirty)
    SELECT id, true FROM old_rows ORDER BY id
    ON CONFLICT (account_id) DO UPDATE
    SET generation = public.scheduler_dirty_account_sources.generation + 1,
        bucket_dirty = true,
        group_cursor = 0,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_groups_source_from_new()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_group_sources (group_id)
    SELECT id FROM new_rows ORDER BY id
    ON CONFLICT (group_id) DO UPDATE
    SET generation = public.scheduler_dirty_group_sources.generation + 1,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_groups_source_from_update()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_group_sources (group_id)
    SELECT id
    FROM (
        SELECT id FROM old_rows
        UNION
        SELECT id FROM new_rows
    ) AS affected
    ORDER BY id
    ON CONFLICT (group_id) DO UPDATE
    SET generation = public.scheduler_dirty_group_sources.generation + 1,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_groups_source_from_old()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_group_sources (group_id)
    SELECT id FROM old_rows ORDER BY id
    ON CONFLICT (group_id) DO UPDATE
    SET generation = public.scheduler_dirty_group_sources.generation + 1,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_memberships_source_from_new()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_membership_sources (account_id, group_id)
    SELECT account_id, group_id FROM new_rows ORDER BY account_id, group_id
    ON CONFLICT (account_id, group_id) DO UPDATE
    SET generation = public.scheduler_dirty_membership_sources.generation + 1,
        group_cursor = 0,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_memberships_source_from_old()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_membership_sources (account_id, group_id)
    SELECT account_id, group_id FROM old_rows ORDER BY account_id, group_id
    ON CONFLICT (account_id, group_id) DO UPDATE
    SET generation = public.scheduler_dirty_membership_sources.generation + 1,
        group_cursor = 0,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

CREATE OR REPLACE FUNCTION scheduler_memberships_source_from_update()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_membership_sources (account_id, group_id)
    SELECT account_id, group_id
    FROM (
        SELECT account_id, group_id FROM old_rows
        UNION
        SELECT account_id, group_id FROM new_rows
    ) AS affected
    ORDER BY account_id, group_id
    ON CONFLICT (account_id, group_id) DO UPDATE
    SET generation = public.scheduler_dirty_membership_sources.generation + 1,
        group_cursor = 0,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

REVOKE ALL ON FUNCTION scheduler_accounts_source_from_insert() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_accounts_source_from_update() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_accounts_source_from_old() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_groups_source_from_new() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_groups_source_from_update() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_groups_source_from_old() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_memberships_source_from_new() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_memberships_source_from_old() FROM PUBLIC;
REVOKE ALL ON FUNCTION scheduler_memberships_source_from_update() FROM PUBLIC;
REVOKE ALL ON scheduler_dirty_account_sources, scheduler_dirty_group_sources,
    scheduler_dirty_membership_sources FROM PUBLIC;

-- Stable names keep retries idempotent while legacy outbox producers remain
-- active during the preparatory rolling-upgrade phase.
DROP TRIGGER IF EXISTS scheduler_accounts_dirty ON accounts;
DROP TRIGGER IF EXISTS aa_scheduler_accounts_dirty ON accounts;
DROP TRIGGER IF EXISTS scheduler_accounts_insert_source ON accounts;
DROP TRIGGER IF EXISTS scheduler_accounts_update_source ON accounts;
DROP TRIGGER IF EXISTS scheduler_accounts_delete_source ON accounts;
CREATE TRIGGER scheduler_accounts_insert_source
AFTER INSERT ON accounts
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_accounts_source_from_insert();
CREATE TRIGGER scheduler_accounts_update_source
AFTER UPDATE ON accounts
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_accounts_source_from_update();
CREATE TRIGGER scheduler_accounts_delete_source
AFTER DELETE ON accounts
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_accounts_source_from_old();

DROP TRIGGER IF EXISTS scheduler_groups_dirty ON groups;
DROP TRIGGER IF EXISTS aa_scheduler_groups_dirty ON groups;
DROP TRIGGER IF EXISTS scheduler_groups_insert_source ON groups;
DROP TRIGGER IF EXISTS scheduler_groups_update_source ON groups;
DROP TRIGGER IF EXISTS scheduler_groups_delete_source ON groups;
CREATE TRIGGER scheduler_groups_insert_source
AFTER INSERT ON groups
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_groups_source_from_new();
CREATE TRIGGER scheduler_groups_update_source
AFTER UPDATE ON groups
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_groups_source_from_update();
CREATE TRIGGER scheduler_groups_delete_source
AFTER DELETE ON groups
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_groups_source_from_old();

DROP TRIGGER IF EXISTS scheduler_account_groups_dirty ON account_groups;
DROP TRIGGER IF EXISTS aa_scheduler_account_groups_dirty ON account_groups;
DROP TRIGGER IF EXISTS scheduler_memberships_insert_source ON account_groups;
DROP TRIGGER IF EXISTS scheduler_memberships_update_source ON account_groups;
DROP TRIGGER IF EXISTS scheduler_memberships_delete_source ON account_groups;
CREATE TRIGGER scheduler_memberships_insert_source
AFTER INSERT ON account_groups
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_memberships_source_from_new();
CREATE TRIGGER scheduler_memberships_update_source
AFTER UPDATE ON account_groups
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_memberships_source_from_update();
CREATE TRIGGER scheduler_memberships_delete_source
AFTER DELETE ON account_groups
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_memberships_source_from_old();
