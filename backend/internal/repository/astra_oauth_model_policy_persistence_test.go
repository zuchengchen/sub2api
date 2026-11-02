//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type astraPolicyCacheFailure struct {
	service.SchedulerCache
	fail error
}

func (c *astraPolicyCacheFailure) SetAccount(context.Context, *service.Account) error { return c.fail }

func TestAstraOAuthModelPolicyPersistenceMonotonicAndDurable(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Add(-time.Minute)
	previous := astraPolicyCacheAccount("degraded", now)
	newer := astraPolicyCacheAccount("healthy", now.Add(time.Second))
	previousJSON, err := json.Marshal(previous.Extra[service.OpenAIAstraPolicyExtraKey])
	require.NoError(t, err)
	for _, kind := range []string{"new", "older", "older_with_unrelated", "database_error", "outbox_error", "cache_error"} {
		t.Run(kind, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			updates := newer.Extra
			if kind == "older" || kind == "older_with_unrelated" {
				older := astraPolicyCacheAccount("healthy", now.Add(-time.Second))
				updates = older.Extra
			}
			if kind == "older_with_unrelated" {
				updates["note"] = "preserved"
			}
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT extra -> \$1.*FOR NO KEY UPDATE`).WithArgs(service.OpenAIAstraPolicyExtraKey, previous.ID).WillReturnRows(sqlmock.NewRows([]string{"extra"}).AddRow(previousJSON))
			expected := updates
			if kind == "older_with_unrelated" {
				expected = map[string]any{"note": "preserved"}
			}
			payload, err := json.Marshal(expected)
			require.NoError(t, err)
			sentinel := errors.New(kind)
			if kind != "older" {
				update := mock.ExpectExec(`UPDATE accounts SET extra =`).WithArgs(string(payload), previous.ID)
				if kind == "database_error" {
					update.WillReturnError(sentinel)
				} else {
					update.WillReturnResult(sqlmock.NewResult(0, 1))
				}
				if kind != "database_error" {
					event := mock.ExpectExec("INSERT INTO scheduler_outbox").WithArgs(service.SchedulerOutboxEventAccountChanged, previous.ID, nil, nil, sqlmock.AnyArg())
					if kind == "outbox_error" {
						event.WillReturnError(sentinel)
					} else {
						event.WillReturnResult(sqlmock.NewResult(1, 1))
					}
				}
			}
			if kind == "database_error" || kind == "outbox_error" {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			var cache service.SchedulerCache
			if kind == "cache_error" {
				cache = &astraPolicyCacheFailure{fail: sentinel}
				raw, _ := json.Marshal(updates)
				mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE .*`).WithArgs(previous.ID).WillReturnRows(updatedAccountRows(previous.ID, string(raw)))
				mock.ExpectQuery(`(?s)account_groups`).WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id", "priority", "created_at"}))
			}
			repo := newAccountRepositoryWithSQL(client, db, cache)
			err = repo.UpdateExtra(ctx, previous.ID, updates)
			if kind == "database_error" || kind == "outbox_error" || kind == "cache_error" {
				require.ErrorIs(t, err, sentinel)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAstraOAuthModelPolicyBulkPreservesOriginalMaps(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)
	_, err := repo.BulkUpdate(context.Background(), []int64{1, 2}, service.AccountBulkUpdate{Credentials: map[string]any{"model_mapping": map[string]any{"gpt-6-astra": "gpt-6-luna"}, "compact_model_mapping": map[string]any{"gpt-6-luna": "gpt-6-astra"}, "unrelated": "keep"}})
	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	require.Contains(t, exec.execQueries[0], "platform = 'openai' AND type = 'oauth'")
	require.Contains(t, exec.execQueries[0], "- 'model_mapping' - 'compact_model_mapping'")
}
