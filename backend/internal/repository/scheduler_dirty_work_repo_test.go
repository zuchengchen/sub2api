package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerSourcePromotionRejectsMissingOwnership(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	promoted, err := NewSchedulerDirtyWorkRepository(db).Promote(context.Background(), nil, 10)
	require.Zero(t, promoted)
	require.ErrorContains(t, err, "active ownership")
}

func TestSchedulerDirtyWorkListIsBoundedAndOrdered(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta(`
		SELECT kind, entity_id, generation, rebuild_buckets, updated_at,
		       failure_count, last_failure_at, last_error, retry_at
		FROM scheduler_dirty_work
		WHERE retry_at <= clock_timestamp()
		ORDER BY retry_at ASC, updated_at ASC, kind ASC, entity_id ASC
		LIMIT $1
	`)).WithArgs(maxDirtyWorkBatchSize).WillReturnRows(
		sqlmock.NewRows([]string{"kind", "entity_id", "generation", "rebuild_buckets", "updated_at", "failure_count", "last_failure_at", "last_error", "retry_at"}).
			AddRow(int16(2), int64(9), int64(4), true, now, 0, nil, "", time.Time{}),
	)

	items, err := NewSchedulerDirtyWorkRepository(db).List(context.Background(), maxDirtyWorkBatchSize+1)
	require.NoError(t, err)
	require.Equal(t, []service.SchedulerDirtyWork{{Kind: 2, EntityID: 9, Generation: 4, RebuildBuckets: true, UpdatedAt: now}}, items)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerDirtyWorkAcknowledgeIsGenerationConditional(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewSchedulerDirtyWorkRepository(db)
	item := service.SchedulerDirtyWork{Kind: 1, EntityID: 42, Generation: 7}
	query := regexp.QuoteMeta(`
		DELETE FROM scheduler_dirty_work
		WHERE kind = $1 AND entity_id = $2 AND generation = $3
		  AND EXISTS (SELECT 1 FROM scheduler_ownership_epoch WHERE singleton AND epoch = $4)
	`)

	// Zero rows is the expected acknowledgement when a producer concurrently
	// advanced generation; the newer dirty state remains in PostgreSQL.
	mock.ExpectExec(query).WithArgs(item.Kind, item.EntityID, item.Generation, int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	owner := &postgresSchedulerOwnership{conn: conn, epoch: 11, ctx: context.Background()}
	acknowledged, err := repo.Acknowledge(context.Background(), owner, item)
	require.NoError(t, err)
	require.False(t, acknowledged)

	mock.ExpectExec(query).WithArgs(item.Kind, item.EntityID, item.Generation, int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	acknowledged, err = repo.Acknowledge(context.Background(), owner, item)
	require.NoError(t, err)
	require.True(t, acknowledged)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerDirtyWorkAcknowledgeRejectsMissingOwnership(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	acknowledged, err := NewSchedulerDirtyWorkRepository(db).Acknowledge(context.Background(), nil, service.SchedulerDirtyWork{Kind: 1, EntityID: 42, Generation: 1})
	require.False(t, acknowledged)
	require.ErrorContains(t, err, "active ownership")
}

func TestSchedulerDirtyWorkRecordFailureIsGenerationConditional(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	repo := NewSchedulerDirtyWorkRepository(db)
	item := service.SchedulerDirtyWork{Kind: 1, EntityID: 42, Generation: 7}
	query := regexp.QuoteMeta(`
		UPDATE scheduler_dirty_work
		SET failure_count = LEAST(failure_count + 1, 31),
		    last_failure_at = clock_timestamp(),
		    last_error = $4,
		    retry_at = clock_timestamp()
		        + make_interval(secs => LEAST(300, 1 << LEAST(failure_count, 8)))
		WHERE kind = $1 AND entity_id = $2 AND generation = $3
		  AND EXISTS (SELECT 1 FROM scheduler_ownership_epoch WHERE singleton AND epoch = $5)
	`)
	mock.ExpectExec(query).WithArgs(item.Kind, item.EntityID, item.Generation, "errors_errorstring", int64(12)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	defer conn.Close()
	owner := &postgresSchedulerOwnership{conn: conn, epoch: 12, ctx: context.Background()}

	recorded, err := repo.RecordFailure(context.Background(), owner, item, errors.New("failed"))
	require.NoError(t, err)
	require.False(t, recorded)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerDirtyWorkPendingStatsUsesWholePopulation(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	now := time.Now().UTC()

	mock.ExpectQuery(regexp.QuoteMeta(`
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
	`)).WillReturnRows(sqlmock.NewRows([]string{"count", "min", "failed", "oldest_failure"}).AddRow(int64(6), now, int64(1), now))
	stats, err := NewSchedulerDirtyWorkRepository(db).PendingStats(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(6), stats.Count)
	require.Equal(t, now, *stats.OldestUpdatedAt)
	require.Equal(t, int64(1), stats.FailedCount)
	require.Equal(t, now, *stats.OldestFailureAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerOwnershipUsesDedicatedSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pg_try_advisory_lock($1, $2)`)).
		WithArgs(lockArg(schedulerAdvisoryLockNamespace), lockArg(schedulerAdvisoryLockID)).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_lock"}).AddRow(true))
	mock.ExpectQuery("INSERT INTO scheduler_ownership_epoch").
		WillReturnRows(sqlmock.NewRows([]string{"epoch"}).AddRow(int64(41)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pg_advisory_unlock($1, $2)`)).
		WithArgs(lockArg(schedulerAdvisoryLockNamespace), lockArg(schedulerAdvisoryLockID)).
		WillReturnRows(sqlmock.NewRows([]string{"pg_advisory_unlock"}).AddRow(true))

	repo := &schedulerOwnershipRepository{db: db, checkInterval: time.Hour}
	owner, acquired, err := repo.TryAcquire(context.Background())
	require.NoError(t, err)
	require.True(t, acquired)
	require.NotNil(t, owner)
	require.Equal(t, int64(41), owner.Epoch())
	require.NoError(t, owner.Close())
	select {
	case <-owner.Lost():
	default:
		t.Fatal("ownership loss was not exposed on close")
	}
	require.ErrorIs(t, owner.Context().Err(), context.Canceled)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerOwnershipConcurrentCloseStoresOneResult(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pg_try_advisory_lock($1, $2)`)).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("INSERT INTO scheduler_ownership_epoch").
		WillReturnRows(sqlmock.NewRows([]string{"epoch"}).AddRow(int64(42)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pg_advisory_unlock($1, $2)`)).
		WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))

	owner, acquired, err := (&schedulerOwnershipRepository{db: db, checkInterval: time.Hour}).TryAcquire(context.Background())
	require.NoError(t, err)
	require.True(t, acquired)

	const callers = 16
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = owner.Close()
		}(i)
	}
	wg.Wait()
	for _, closeErr := range errs {
		require.NoError(t, closeErr)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerOwnershipUnlockFailureDiscardsSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pg_try_advisory_lock($1, $2)`)).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("INSERT INTO scheduler_ownership_epoch").
		WillReturnRows(sqlmock.NewRows([]string{"epoch"}).AddRow(int64(43)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pg_advisory_unlock($1, $2)`)).
		WillReturnError(errors.New("unlock failed"))
	mock.ExpectClose()

	owner, acquired, err := (&schedulerOwnershipRepository{db: db, checkInterval: time.Hour}).TryAcquire(context.Background())
	require.NoError(t, err)
	require.True(t, acquired)
	firstErr := owner.Close()
	require.ErrorContains(t, firstErr, "unlock failed")
	require.Equal(t, firstErr, owner.Close())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerOwnershipConnectionLossIsSurfacedAndDiscarded(t *testing.T) {
	lostErr := errors.New("connection lost")
	db, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pg_try_advisory_lock($1, $2)`)).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("INSERT INTO scheduler_ownership_epoch").
		WillReturnRows(sqlmock.NewRows([]string{"epoch"}).AddRow(int64(44)))
	mock.ExpectPing().WillReturnError(lostErr)
	mock.ExpectClose()

	owner, acquired, err := (&schedulerOwnershipRepository{db: db, checkInterval: time.Millisecond}).TryAcquire(context.Background())
	require.NoError(t, err)
	require.True(t, acquired)
	select {
	case <-owner.Lost():
	case <-time.After(time.Second):
		t.Fatal("ownership loss was not bounded")
	}
	require.ErrorContains(t, owner.Err(), lostErr.Error())
	require.ErrorContains(t, owner.Close(), lostErr.Error())
	require.NoError(t, mock.ExpectationsWereMet())
}

func lockArg(v int32) driver.Value { return int64(v) }
