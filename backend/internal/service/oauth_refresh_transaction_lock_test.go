//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

type openAITransactionLockCache struct {
	refreshAPICacheStub
	acquireCalls int
}

func (cache *openAITransactionLockCache) AcquireRefreshLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	cache.acquireCalls++
	return cache.refreshAPICacheStub.AcquireRefreshLock(ctx, key, ttl)
}

func TestOpenAIRefreshEntTransactionDoesNotWaitOnLocalRefreshLock(t *testing.T) {
	account, _, executor, api, _ := newOpenAIRefreshCASFixture()
	cache := &openAITransactionLockCache{refreshAPICacheStub: refreshAPICacheStub{lockResult: true}}
	api.tokenCache = cache
	local := api.getLocalLock(OpenAITokenCacheKey(account))
	require.NoError(t, local.Lock(context.Background()))
	defer local.Unlock()

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	mock.ExpectBegin()
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	mock.ExpectRollback()
	defer func() { require.NoError(t, tx.Rollback()) }()

	// The scoped-admin caller already owns accounts' SQL row lock. The local
	// owner can be a background refresh blocked on that row at its CAS UPDATE.
	// A held local lock must therefore return LockHeld without reaching the
	// request deadline or contacting the distributed lock / upstream.
	deadline, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	ctx := dbent.NewTxContext(deadline, tx)
	result, refreshErr := api.RefreshIfNeeded(ctx, account, forceOpenAIRefreshForTest(executor, account), 0)
	require.NoError(t, refreshErr)
	require.NoError(t, deadline.Err(), "transaction must unwind before the request deadline breaks the lock inversion")
	require.NotNil(t, result)
	require.True(t, result.LockHeld)
	require.Zero(t, executor.calls.Load())
	require.Zero(t, cache.acquireCalls)
	require.Zero(t, cache.releaseCalls)
}

func TestOpenAIRefreshEntTransactionReturnsQuicklyWhenDistributedLockIsHeld(t *testing.T) {
	account, _, executor, api, _ := newOpenAIRefreshCASFixture()
	cache := &openAITransactionLockCache{refreshAPICacheStub: refreshAPICacheStub{lockResult: false}}
	api.tokenCache = cache
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	mock.ExpectBegin()
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	deadline, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	ctx := dbent.NewTxContext(deadline, tx)

	result, refreshErr := api.RefreshIfNeeded(ctx, account, forceOpenAIRefreshForTest(executor, account), 0)
	require.NoError(t, refreshErr)
	require.NoError(t, deadline.Err())
	require.NotNil(t, result)
	require.True(t, result.LockHeld)
	require.Equal(t, 1, cache.acquireCalls)
	require.Zero(t, cache.releaseCalls, "the distributed lock belongs to another instance")
	require.Zero(t, executor.calls.Load())

	// The acquired local lock remains coordinated with the outer transaction,
	// then is released on rollback. A later redundant rollback cannot release
	// it twice (which would block receiving a second mutex token).
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.Error(t, tx.Rollback())
	local := api.getLocalLock(OpenAITokenCacheKey(account))
	lockCtx, lockStop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer lockStop()
	require.NoError(t, local.Lock(lockCtx))
	local.Unlock()
	require.Zero(t, cache.releaseCalls)
	require.NoError(t, mock.ExpectationsWereMet())
}
