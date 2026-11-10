package repository

import (
	"context"
	"errors"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRepository_UpdateOpenAIOAuthCredentialsIfUnchanged_ExactSnapshotAndAtomicOutbox(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(1)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	proxy := int64(23)
	applied, err := repo.UpdateOpenAIOAuthCredentialsIfUnchanged(context.Background(), 42, map[string]any{"refresh_token": "observed", "_token_version": int64(10)}, &proxy, map[string]any{"refresh_token": "rotated", "_token_version": int64(11)})
	require.NoError(t, err)
	require.True(t, applied)
	require.Len(t, exec.execQueries, 1)
	normalized := normalizeSQLWhitespace(exec.execQueries[0])
	for _, fragment := range []string{"WITH updated AS", "a.deleted_at IS NULL", "a.platform = $3", "a.type = $4", "credentials = $5::jsonb", "proxy_id IS NOT DISTINCT FROM $6", "INSERT INTO scheduler_outbox"} {
		require.Contains(t, normalized, fragment)
	}
	require.Len(t, exec.execArgs[0], 7)
	require.Equal(t, service.PlatformOpenAI, exec.execArgs[0][2])
	require.Equal(t, &proxy, exec.execArgs[0][5])
}

func TestAccountRepository_UpdateOpenAIOAuthCredentialsIfUnchanged_MissAndWriteError(t *testing.T) {
	for _, mode := range []string{"miss", "error"} {
		t.Run(mode, func(t *testing.T) {
			exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
			if mode == "error" {
				exec.err = errors.New("synthetic outbox failure")
			}
			applied, err := newAccountRepositoryWithSQL(nil, exec, nil).UpdateOpenAIOAuthCredentialsIfUnchanged(context.Background(), 42, map[string]any{}, nil, map[string]any{"access_token": "rotated"})
			require.False(t, applied)
			if mode == "error" {
				require.ErrorIs(t, err, exec.err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, exec.execQueries, 1)
		})
	}
	var missing *accountRepository
	applied, err := missing.UpdateOpenAIOAuthCredentialsIfUnchanged(context.Background(), 42, nil, nil, nil)
	require.False(t, applied)
	require.Error(t, err)
}

func TestAccountRepository_UpdateOpenAIOAuthCredentialsIfUnchanged_UsesEnclosingTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	mock.ExpectBegin()
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	// Any accidental non-transactional CAS would hit this recorder instead.
	outside := &recordingSQLExecutor{err: errors.New("non-transactional write forbidden")}
	repo := newAccountRepositoryWithSQL(client, outside, nil)
	mock.ExpectExec("WITH updated AS").WithArgs(`{"access_token":"rotated"}`, int64(42), service.PlatformOpenAI, service.AccountTypeOAuth, `{"access_token":"observed"}`, nil, service.SchedulerOutboxEventAccountChanged).WillReturnResult(sqlmock.NewResult(0, 1))
	applied, err := repo.UpdateOpenAIOAuthCredentialsIfUnchanged(dbent.NewTxContext(context.Background(), tx), 42, map[string]any{"access_token": "observed"}, nil, map[string]any{"access_token": "rotated"})
	require.NoError(t, err)
	require.True(t, applied)
	require.Empty(t, outside.execQueries)
	mock.ExpectRollback()
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}
