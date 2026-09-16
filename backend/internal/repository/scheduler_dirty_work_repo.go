package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	defaultDirtyWorkBatchSize      = 100
	maxDirtyWorkBatchSize          = 1000
	maxDirtyWorkFailureText        = 512
	schedulerOwnershipProbeTimeout = 5 * time.Second

	// Two-key form avoids coupling the lock identity to PostgreSQL hash funcs.
	schedulerAdvisoryLockNamespace int32 = 0x53324150 // "S2AP"
	schedulerAdvisoryLockID        int32 = 1
)

type schedulerDirtyWorkRepository struct{ db *sql.DB }

func NewSchedulerDirtyWorkRepository(db *sql.DB) service.SchedulerDirtyWorkRepository {
	return &schedulerDirtyWorkRepository{db: db}
}

type dirtySourceKind int16

const (
	dirtyAccountSource dirtySourceKind = iota + 1
	dirtyMembershipSource
	dirtyGroupSource
)

// Promote handles one source kind per transaction. This bounds row locks and
// ensures every transaction acquires source and canonical rows in one order.
func (r *schedulerDirtyWorkRepository) Promote(ctx context.Context, ownership service.SchedulerOwnership, limit int) (int, error) {
	owner, ok := ownership.(*postgresSchedulerOwnership)
	if !ok || owner == nil || owner.Context().Err() != nil {
		return 0, errors.New("scheduler source promotion requires active ownership")
	}
	if limit <= 0 {
		limit = defaultDirtyWorkBatchSize
	} else if limit > maxDirtyWorkBatchSize {
		limit = maxDirtyWorkBatchSize
	}

	total := 0
	for _, kind := range []dirtySourceKind{dirtyAccountSource, dirtyMembershipSource, dirtyGroupSource} {
		promoted, err := r.promoteSourceKind(ctx, owner, kind, limit)
		if err != nil {
			return total, err
		}
		total += promoted
	}
	return total, nil
}

func (r *schedulerDirtyWorkRepository) promoteSourceKind(ctx context.Context, owner *postgresSchedulerOwnership, kind dirtySourceKind, limit int) (int, error) {
	owner.opMu.Lock()
	defer owner.opMu.Unlock()
	if owner.Context().Err() != nil {
		return 0, errors.New("scheduler source promotion requires active ownership")
	}

	// Couple caller cancellation with ownership loss. Close cancels this context
	// before waiting for opMu, so a stale owner cannot commit during takeover.
	promoteCtx, cancel := context.WithCancel(ctx)
	stopOwnershipCancel := context.AfterFunc(owner.Context(), cancel)
	defer func() {
		stopOwnershipCancel()
		cancel()
	}()

	// Use the lock-holding session. If it disconnects, PostgreSQL rolls this
	// transaction back before another replica can acquire scheduler ownership.
	tx, err := owner.conn.BeginTx(promoteCtx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var epochCurrent bool
	if err := tx.QueryRowContext(promoteCtx, `SELECT EXISTS (
		SELECT 1 FROM scheduler_ownership_epoch WHERE singleton AND epoch = $1
	)`, owner.epoch).Scan(&epochCurrent); err != nil {
		return 0, err
	}
	if !epochCurrent {
		return 0, errors.New("scheduler ownership epoch is stale during source promotion")
	}

	var promoted int
	switch kind {
	case dirtyAccountSource:
		err = tx.QueryRowContext(promoteCtx, promoteAccountSourcesSQL, limit).Scan(&promoted)
	case dirtyMembershipSource:
		err = tx.QueryRowContext(promoteCtx, promoteMembershipSourcesSQL, limit).Scan(&promoted)
	case dirtyGroupSource:
		err = tx.QueryRowContext(promoteCtx, promoteGroupSourcesSQL, limit).Scan(&promoted)
	default:
		return 0, fmt.Errorf("unknown scheduler source kind %d", kind)
	}
	if err != nil {
		return 0, err
	}
	if owner.Context().Err() != nil {
		return 0, errors.New("scheduler ownership lost during source promotion")
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return promoted, nil
}

const promoteAccountSourcesSQL = `
WITH picked AS MATERIALIZED (
    SELECT account_id, generation, bucket_dirty, group_cursor
    FROM scheduler_dirty_account_sources
    ORDER BY updated_at, account_id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
), group_page AS MATERIALIZED (
    SELECT ag.group_id
    FROM picked AS p
    JOIN account_groups AS ag ON ag.account_id = p.account_id
    WHERE p.bucket_dirty AND ag.group_id > p.group_cursor
    ORDER BY ag.group_id
    LIMIT $1
), scopes AS MATERIALIZED (
    SELECT 1::smallint AS kind, account_id AS entity_id, false AS rebuild_buckets
    FROM picked WHERE group_cursor = 0
    UNION
    SELECT 2::smallint, 0, true FROM picked WHERE bucket_dirty AND group_cursor = 0
    UNION
    SELECT 2::smallint, group_id, true FROM group_page
), promoted AS (
    INSERT INTO scheduler_dirty_work (kind, entity_id, rebuild_buckets)
    SELECT kind, entity_id, rebuild_buckets FROM scopes ORDER BY kind, entity_id
    ON CONFLICT (kind, entity_id) DO UPDATE
    SET generation = scheduler_dirty_work.generation + 1,
        rebuild_buckets = scheduler_dirty_work.rebuild_buckets OR EXCLUDED.rebuild_buckets,
        failure_count = 0,
        last_failure_at = NULL,
        last_error = '',
        retry_at = clock_timestamp(),
        updated_at = clock_timestamp()
), advanced AS (
    UPDATE scheduler_dirty_account_sources AS source
    SET group_cursor = page.max_group_id,
        updated_at = clock_timestamp()
    FROM picked,
         LATERAL (SELECT max(group_id) AS max_group_id FROM group_page) AS page
    WHERE source.account_id = picked.account_id
      AND source.generation = picked.generation
      AND picked.bucket_dirty
      AND page.max_group_id IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM account_groups AS remaining
          WHERE remaining.account_id = picked.account_id
            AND remaining.group_id > page.max_group_id
      )
    RETURNING 1
), removed AS (
    DELETE FROM scheduler_dirty_account_sources AS source
    USING picked
    WHERE source.account_id = picked.account_id
      AND source.generation = picked.generation
      AND NOT EXISTS (SELECT 1 FROM advanced)
    RETURNING 1
)
SELECT count(*) FROM picked
`

const promoteMembershipSourcesSQL = `
WITH picked AS MATERIALIZED (
    SELECT account_id, group_id, generation, group_cursor
    FROM scheduler_dirty_membership_sources
    ORDER BY updated_at, account_id, group_id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
), group_page AS MATERIALIZED (
    SELECT ag.group_id
    FROM picked
    JOIN account_groups AS ag USING (account_id)
    WHERE ag.group_id > picked.group_cursor
    ORDER BY ag.group_id
    LIMIT $1
), scopes AS MATERIALIZED (
    SELECT 1::smallint AS kind, account_id AS entity_id, false AS rebuild_buckets
    FROM picked WHERE group_cursor = 0
    UNION
    SELECT 2::smallint, 0, true FROM picked WHERE group_cursor = 0
    UNION
    SELECT 2::smallint, group_id, true FROM picked WHERE group_cursor = 0
    UNION
    SELECT 2::smallint, group_id, true FROM group_page
), promoted AS (
    INSERT INTO scheduler_dirty_work (kind, entity_id, rebuild_buckets)
    SELECT kind, entity_id, rebuild_buckets FROM scopes ORDER BY kind, entity_id
    ON CONFLICT (kind, entity_id) DO UPDATE
    SET generation = scheduler_dirty_work.generation + 1,
        rebuild_buckets = scheduler_dirty_work.rebuild_buckets OR EXCLUDED.rebuild_buckets,
        failure_count = 0,
        last_failure_at = NULL,
        last_error = '',
        retry_at = clock_timestamp(),
        updated_at = clock_timestamp()
), advanced AS (
    UPDATE scheduler_dirty_membership_sources AS source
    SET group_cursor = page.max_group_id,
        updated_at = clock_timestamp()
    FROM picked,
         LATERAL (SELECT max(group_id) AS max_group_id FROM group_page) AS page
    WHERE source.account_id = picked.account_id
      AND source.group_id = picked.group_id
      AND source.generation = picked.generation
      AND page.max_group_id IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM account_groups AS remaining
          WHERE remaining.account_id = picked.account_id
            AND remaining.group_id > page.max_group_id
      )
    RETURNING 1
), removed AS (
    DELETE FROM scheduler_dirty_membership_sources AS source
    USING picked
    WHERE source.account_id = picked.account_id
      AND source.group_id = picked.group_id
      AND source.generation = picked.generation
      AND NOT EXISTS (SELECT 1 FROM advanced)
    RETURNING 1
)
SELECT count(*) FROM picked
`

const promoteGroupSourcesSQL = `
WITH picked AS MATERIALIZED (
    SELECT group_id, generation
    FROM scheduler_dirty_group_sources
    ORDER BY updated_at, group_id
    LIMIT $1
    FOR UPDATE SKIP LOCKED
), promoted AS (
    INSERT INTO scheduler_dirty_work (kind, entity_id, rebuild_buckets)
    SELECT 2::smallint, group_id, true FROM picked ORDER BY group_id
    ON CONFLICT (kind, entity_id) DO UPDATE
    SET generation = scheduler_dirty_work.generation + 1,
        rebuild_buckets = true,
        failure_count = 0,
        last_failure_at = NULL,
        last_error = '',
        retry_at = clock_timestamp(),
        updated_at = clock_timestamp()
), removed AS (
    DELETE FROM scheduler_dirty_group_sources AS source
    USING picked
    WHERE source.group_id = picked.group_id
      AND source.generation = picked.generation
    RETURNING 1
)
SELECT count(*) FROM removed
`

func (r *schedulerDirtyWorkRepository) List(ctx context.Context, limit int) ([]service.SchedulerDirtyWork, error) {
	if limit <= 0 {
		limit = defaultDirtyWorkBatchSize
	} else if limit > maxDirtyWorkBatchSize {
		limit = maxDirtyWorkBatchSize
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT kind, entity_id, generation, rebuild_buckets, updated_at,
		       failure_count, last_failure_at, last_error, retry_at
		FROM scheduler_dirty_work
		WHERE retry_at <= clock_timestamp()
		ORDER BY retry_at ASC, updated_at ASC, kind ASC, entity_id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	work := make([]service.SchedulerDirtyWork, 0, limit)
	for rows.Next() {
		var item service.SchedulerDirtyWork
		var lastFailure sql.NullTime
		if err := rows.Scan(
			&item.Kind,
			&item.EntityID,
			&item.Generation,
			&item.RebuildBuckets,
			&item.UpdatedAt,
			&item.FailureCount,
			&lastFailure,
			&item.LastError,
			&item.RetryAt,
		); err != nil {
			return nil, err
		}
		if lastFailure.Valid {
			item.LastFailureAt = &lastFailure.Time
		}
		work = append(work, item)
	}
	return work, rows.Err()
}

func (r *schedulerDirtyWorkRepository) RequestFullRebuild(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO scheduler_dirty_work (kind, entity_id, rebuild_buckets)
		VALUES (3, 0, true)
		ON CONFLICT (kind, entity_id) DO UPDATE
		SET generation = scheduler_dirty_work.generation + 1,
		    rebuild_buckets = true,
		    failure_count = 0,
		    last_failure_at = NULL,
		    last_error = '',
		    retry_at = clock_timestamp(),
		    updated_at = clock_timestamp()
	`)
	return err
}

func (r *schedulerDirtyWorkRepository) RecordFailure(ctx context.Context, ownership service.SchedulerOwnership, work service.SchedulerDirtyWork, failure error) (bool, error) {
	owner, ok := ownership.(*postgresSchedulerOwnership)
	if !ok || owner == nil || owner.Context().Err() != nil {
		return false, errors.New("scheduler dirty failure recording requires active ownership")
	}
	owner.opMu.Lock()
	defer owner.opMu.Unlock()
	if owner.Context().Err() != nil {
		return false, errors.New("scheduler dirty failure recording requires active ownership")
	}
	failureCtx, cancel := context.WithCancel(ctx)
	stopOwnershipCancel := context.AfterFunc(owner.Context(), cancel)
	defer func() { stopOwnershipCancel(); cancel() }()

	message := schedulerDirtyFailureClass(failure)
	result, err := owner.conn.ExecContext(failureCtx, `
		UPDATE scheduler_dirty_work
		SET failure_count = LEAST(failure_count + 1, 31),
		    last_failure_at = clock_timestamp(),
		    last_error = $4,
		    retry_at = clock_timestamp()
		        + make_interval(secs => LEAST(300, 1 << LEAST(failure_count, 8)))
		WHERE kind = $1 AND entity_id = $2 AND generation = $3
		  AND EXISTS (SELECT 1 FROM scheduler_ownership_epoch WHERE singleton AND epoch = $5)
	`, work.Kind, work.EntityID, work.Generation, message, owner.epoch)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 1 {
		return false, fmt.Errorf("scheduler dirty failure recording affected %d rows", n)
	}
	return n == 1, nil
}

func schedulerDirtyFailureClass(failure error) string {
	switch {
	case failure == nil:
		return "scheduler_rebuild_failed"
	case errors.Is(failure, context.Canceled):
		return "context_canceled"
	case errors.Is(failure, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(failure, sql.ErrConnDone), errors.Is(failure, driver.ErrBadConn):
		return "database_unavailable"
	}

	// Error text can contain credentials, URLs, or account metadata. Persist only
	// a bounded class derived from the concrete error type, never failure.Error().
	failureType := strings.ToLower(fmt.Sprintf("%T", failure))
	var class strings.Builder
	class.Grow(len(failureType))
	lastUnderscore := false
	for _, r := range failureType {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			class.WriteRune(r)
			lastUnderscore = false
		case class.Len() > 0 && !lastUnderscore:
			class.WriteByte('_')
			lastUnderscore = true
		}
		if class.Len() >= maxDirtyWorkFailureText {
			break
		}
	}
	value := strings.Trim(class.String(), "_")
	if value == "" {
		return "scheduler_rebuild_failed"
	}
	return value
}

func (r *schedulerDirtyWorkRepository) Acknowledge(ctx context.Context, ownership service.SchedulerOwnership, work service.SchedulerDirtyWork) (bool, error) {
	owner, ok := ownership.(*postgresSchedulerOwnership)
	if !ok || owner == nil || owner.Context().Err() != nil {
		return false, errors.New("scheduler dirty acknowledgement requires active ownership")
	}
	owner.opMu.Lock()
	defer owner.opMu.Unlock()
	if owner.Context().Err() != nil {
		return false, errors.New("scheduler dirty acknowledgement requires active ownership")
	}
	ackCtx, cancel := context.WithCancel(ctx)
	stopOwnershipCancel := context.AfterFunc(owner.Context(), cancel)
	defer func() { stopOwnershipCancel(); cancel() }()

	result, err := owner.conn.ExecContext(ackCtx, `
		DELETE FROM scheduler_dirty_work
		WHERE kind = $1 AND entity_id = $2 AND generation = $3
		  AND EXISTS (SELECT 1 FROM scheduler_ownership_epoch WHERE singleton AND epoch = $4)
	`, work.Kind, work.EntityID, work.Generation, owner.epoch)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 1 {
		return false, fmt.Errorf("scheduler dirty acknowledgement affected %d rows", n)
	}
	return n == 1, nil
}

func (r *schedulerDirtyWorkRepository) PendingStats(ctx context.Context) (service.SchedulerDirtyWorkStats, error) {
	var stats service.SchedulerDirtyWorkStats
	var oldest, oldestFailure sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(pending_count), 0), MIN(oldest_updated_at),
		       COALESCE(SUM(failed_count), 0), MIN(oldest_failure_at)
		FROM (
			SELECT COUNT(*) AS pending_count, MIN(updated_at) AS oldest_updated_at,
			       COUNT(*) FILTER (WHERE failure_count > 0) AS failed_count,
			       MIN(last_failure_at) AS oldest_failure_at
			FROM scheduler_dirty_work
			UNION ALL
			SELECT COUNT(*), MIN(updated_at), 0, NULL::timestamptz
			FROM scheduler_dirty_account_sources
			UNION ALL
			SELECT COUNT(*), MIN(updated_at), 0, NULL::timestamptz
			FROM scheduler_dirty_membership_sources
			UNION ALL
			SELECT COUNT(*), MIN(updated_at), 0, NULL::timestamptz
			FROM scheduler_dirty_group_sources
		) AS pending
	`).Scan(&stats.Count, &oldest, &stats.FailedCount, &oldestFailure)
	if oldest.Valid {
		stats.OldestUpdatedAt = &oldest.Time
	}
	if oldestFailure.Valid {
		stats.OldestFailureAt = &oldestFailure.Time
	}
	return stats, err
}

type schedulerOwnershipRepository struct {
	db            *sql.DB
	checkInterval time.Duration
}

func NewSchedulerOwnershipRepository(db *sql.DB) service.SchedulerOwnershipRepository {
	return &schedulerOwnershipRepository{db: db, checkInterval: time.Second}
}

func (r *schedulerOwnershipRepository) TryAcquire(ctx context.Context) (service.SchedulerOwnership, bool, error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1, $2)`, schedulerAdvisoryLockNamespace, schedulerAdvisoryLockID).Scan(&acquired); err != nil {
		// The server may have acquired the session lock before the result became
		// unreadable. Do not return an ownership-ambiguous session to the pool.
		_ = discardSQLConn(conn)
		return nil, false, err
	}
	if !acquired {
		_ = conn.Close()
		return nil, false, nil
	}
	var epoch int64
	if err := conn.QueryRowContext(ctx, `
		INSERT INTO scheduler_ownership_epoch (singleton, epoch)
		VALUES (true, 1)
		ON CONFLICT (singleton) DO UPDATE
		SET epoch = scheduler_ownership_epoch.epoch + 1
		RETURNING epoch
	`).Scan(&epoch); err != nil {
		_ = discardSQLConn(conn)
		return nil, false, err
	}

	ownerCtx, cancel := context.WithCancel(context.Background())
	owner := &postgresSchedulerOwnership{conn: conn, epoch: epoch, ctx: ownerCtx, cancel: cancel, lost: make(chan struct{}), done: make(chan struct{})}
	go owner.monitor(r.checkInterval)
	return owner, true, nil
}

type postgresSchedulerOwnership struct {
	conn       *sql.Conn
	epoch      int64
	opMu       sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
	lost       chan struct{}
	done       chan struct{}
	finishOnce sync.Once
	closeOnce  sync.Once
	mu         sync.RWMutex
	err        error
	closeErr   error
}

func (o *postgresSchedulerOwnership) Epoch() int64             { return o.epoch }
func (o *postgresSchedulerOwnership) Context() context.Context { return o.ctx }
func (o *postgresSchedulerOwnership) Lost() <-chan struct{}    { return o.lost }
func (o *postgresSchedulerOwnership) Err() error {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.err
}

func (o *postgresSchedulerOwnership) monitor(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(o.done)
	for {
		select {
		case <-o.ctx.Done():
			return
		case <-ticker.C:
			o.opMu.Lock()
			if o.ctx.Err() != nil {
				o.opMu.Unlock()
				return
			}
			probeCtx, cancel := context.WithTimeout(o.ctx, schedulerOwnershipProbeTimeout)
			err := o.conn.PingContext(probeCtx)
			cancel()
			o.opMu.Unlock()
			if err != nil {
				o.finish(fmt.Errorf("scheduler ownership connection lost: %w", err))
				return
			}
		}
	}
}

func (o *postgresSchedulerOwnership) finish(err error) {
	o.finishOnce.Do(func() {
		o.mu.Lock()
		o.err = err
		o.mu.Unlock()
		close(o.lost)
		o.cancel()
	})
}

func (o *postgresSchedulerOwnership) Close() error {
	o.closeOnce.Do(func() {
		o.finish(context.Canceled)
		<-o.done // PingContext is bounded and is canceled by finish.

		o.mu.RLock()
		terminalErr := o.err
		o.mu.RUnlock()
		if !errors.Is(terminalErr, context.Canceled) {
			o.closeErr = errors.Join(terminalErr, discardSQLConn(o.conn))
			return
		}

		// A healthy sql.Conn.Close returns its physical session to the pool.
		// Unlock first; if the outcome is uncertain, discard that session.
		unlockCtx, cancel := context.WithTimeout(context.Background(), schedulerOwnershipProbeTimeout)
		var unlocked bool
		unlockErr := o.conn.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock($1, $2)`, schedulerAdvisoryLockNamespace, schedulerAdvisoryLockID).Scan(&unlocked)
		cancel()
		if unlockErr != nil {
			o.closeErr = errors.Join(fmt.Errorf("release scheduler ownership: %w", unlockErr), discardSQLConn(o.conn))
			return
		}
		if !unlocked {
			o.closeErr = errors.Join(errors.New("scheduler ownership advisory lock was not held"), discardSQLConn(o.conn))
			return
		}
		if err := o.conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			o.closeErr = err
		}
	})
	return o.closeErr
}

// discardSQLConn uses database/sql's supported bad-connection signal. Returning
// driver.ErrBadConn from Raw makes database/sql close the physical driver
// connection instead of placing it back in the idle pool.
func discardSQLConn(conn *sql.Conn) error {
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if err != nil && !errors.Is(err, driver.ErrBadConn) && !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("discard scheduler ownership connection: %w", err)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("close discarded scheduler ownership connection: %w", err)
	}
	return nil
}
