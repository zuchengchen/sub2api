//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/alitto/pond/v2"
	"github.com/stretchr/testify/require"
)

func TestCreateAccountStartsOpenAIImportChecksAfterCommit(t *testing.T) {
	for _, mode := range []string{"commit", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			mock.ExpectBegin()
			tx, err := client.Tx(t.Context())
			require.NoError(t, err)
			request, cancel := context.WithCancel(t.Context())
			defer cancel()

			repo := &upstreamBillingProbeAccountRepo{}
			var steps []string
			var quotaID, tiboID int64
			done := make(chan struct{})
			checks := &OpenAIAccountImportChecks{
				accounts: repo,
				quota: quotaRefreshTestQuery(func(ctx context.Context, id int64) error {
					steps = append(steps, "quota")
					quotaID = id
					if ctx.Err() != nil || dbent.TxFromContext(ctx) != nil {
						steps = append(steps, "stale context")
					}
					return errors.New("upstream quota unavailable")
				}),
				probeTibo: func(_ context.Context, account *Account) {
					steps = append(steps, "tibo")
					tiboID = account.ID
					close(done)
				},
				pool: pond.NewPool(1),
				ctx:  t.Context(),
			}
			defer checks.pool.StopAndWait()
			admin := &adminServiceImpl{accountRepo: repo, openAIImportChecks: checks}
			account, err := admin.CreateAccount(dbent.NewTxContext(request, tx), &CreateAccountInput{
				Name:                 "Imported GPT",
				Platform:             PlatformOpenAI,
				Type:                 AccountTypeOAuth,
				SkipDefaultGroupBind: true,
			})
			require.NoError(t, err)
			require.Zero(t, checks.pool.SubmittedTasks())

			cancel()
			if mode == "commit" {
				mock.ExpectCommit()
				require.NoError(t, tx.Commit())
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("import checks did not complete")
				}
				require.Equal(t, []string{"quota", "tibo"}, steps)
				require.Equal(t, account.ID, quotaID)
				require.Equal(t, account.ID, tiboID)
				require.EqualValues(t, 1, checks.pool.SubmittedTasks())
			} else {
				mock.ExpectRollback()
				require.NoError(t, tx.Rollback())
				require.Zero(t, checks.pool.SubmittedTasks())
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
