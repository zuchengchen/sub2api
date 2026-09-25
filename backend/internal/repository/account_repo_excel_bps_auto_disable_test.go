package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestDisableExcelBPSOn403AtomicWrite(t *testing.T) {
	for _, tc := range []struct {
		name       string
		affected   int64
		failure    string
		wantChange bool
	}{
		{name: "changed", affected: 1, wantChange: true},
		{name: "stale or already disabled"},
		{name: "begin failure", failure: "begin"},
		{name: "update failure", failure: "update"},
		{name: "affected rows failure", failure: "rows"},
		{name: "outbox failure", affected: 1, failure: "outbox"},
		{name: "commit failure", affected: 1, failure: "commit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			failure := errors.New("database unavailable")
			begin := mock.ExpectBegin()
			if tc.failure == "begin" {
				begin.WillReturnError(failure)
			} else {
				query := `(?s)` + regexp.QuoteMeta("UPDATE accounts") + `.*` +
					regexp.QuoteMeta("SET extra = jsonb_set(extra, '{openai_excel_bps}', 'false'::jsonb), updated_at = NOW()") + `.*` +
					regexp.QuoteMeta("WHERE id = $1 AND deleted_at IS NULL AND parent_account_id IS NULL") + `.*` +
					regexp.QuoteMeta("AND platform = 'openai' AND type = 'oauth'") + `.*` +
					regexp.QuoteMeta("AND credentials = $2::jsonb") + `.*` +
					regexp.QuoteMeta("AND extra -> 'openai_excel_bps' = 'true'::jsonb") + `.*` +
					regexp.QuoteMeta("AND extra -> 'openai_excel_bps_auto_disable_on_403' = 'true'::jsonb")
				update := mock.ExpectExec(query).WithArgs(int64(27), `{"access_token":"test-token"}`)
				switch tc.failure {
				case "update":
					update.WillReturnError(failure)
				case "rows":
					update.WillReturnResult(sqlmock.NewErrorResult(failure))
				default:
					update.WillReturnResult(sqlmock.NewResult(0, tc.affected))
				}
				if tc.affected > 0 {
					outbox := mock.ExpectExec(regexp.QuoteMeta("INSERT INTO scheduler_outbox")).
						WithArgs(service.SchedulerOutboxEventAccountChanged, int64(27), nil, nil, sqlmock.AnyArg())
					if tc.failure == "outbox" {
						outbox.WillReturnError(failure)
					} else {
						outbox.WillReturnResult(sqlmock.NewResult(1, 1))
					}
				}
				switch tc.failure {
				case "update", "rows", "outbox":
					mock.ExpectRollback()
				case "commit":
					mock.ExpectCommit().WillReturnError(failure)
				default:
					mock.ExpectCommit()
				}
			}
			repo := newAccountRepositoryWithSQL(client, db, nil)
			account := &service.Account{ID: 27, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test-token"},
				Extra:       map[string]any{"openai_excel_bps": true, "openai_excel_bps_auto_disable_on_403": true}}
			changed, err := repo.DisableExcelBPSOn403(context.Background(), account)
			if tc.failure != "" {
				require.ErrorIs(t, err, failure)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantChange, changed)
			require.True(t, account.IsExcelBPSEnabled())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
