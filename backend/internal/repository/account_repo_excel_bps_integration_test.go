//go:build integration

package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newExcelBPSAutoDisableAccount() *service.Account {
	return &service.Account{
		Name: "bps-auto-disable-test", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, Concurrency: 10,
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"},
		Extra: map[string]any{
			"openai_excel_bps": true, "openai_excel_bps_auto_disable_on_403": true,
			"openai_excel_bps_models":                  []string{"gpt-6-astra"},
			"openai_excel_bps_cache_creation_as_input": true, "openai_passthrough": true,
		},
	}
}

func TestDisableExcelBPSOn403HonorsCurrentSettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"opt-in withdrawn", `UPDATE accounts SET extra = extra || '{"openai_excel_bps_auto_disable_on_403":false}' WHERE id = $1`},
		{"already disabled", `UPDATE accounts SET extra = extra || '{"openai_excel_bps":false}' WHERE id = $1`},
		{"credentials replaced", `UPDATE accounts SET credentials = '{"access_token":"new-token"}' WHERE id = $1`},
		{"account deleted", `UPDATE accounts SET deleted_at = NOW() WHERE id = $1`},
		{"type changed", `UPDATE accounts SET type = 'apikey' WHERE id = $1`},
		{"invalid boolean", `UPDATE accounts SET extra = extra || '{"openai_excel_bps_auto_disable_on_403":"true"}' WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := testEntTx(t)
			ctx := dbent.NewTxContext(context.Background(), tx)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			account := mustCreateAccount(t, tx.Client(), newExcelBPSAutoDisableAccount())
			_, err := tx.Client().ExecContext(ctx, tc.query, account.ID)
			require.NoError(t, err)
			countEvents := func() int {
				rows, err := tx.Client().QueryContext(ctx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", account.ID)
				require.NoError(t, err)
				defer func() { _ = rows.Close() }()
				require.True(t, rows.Next())
				var count int
				require.NoError(t, rows.Scan(&count))
				require.NoError(t, rows.Err())
				return count
			}
			before := countEvents()
			changed, err := repo.DisableExcelBPSOn403(ctx, account)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, before, countEvents(), "no routing event for a skipped write")
		})
	}
}

func TestDisableExcelBPSOn403ConcurrentAndCache(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	account := mustCreateAccount(t, client, newExcelBPSAutoDisableAccount())
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		require.NoError(t, err)
		require.NoError(t, client.Account.DeleteOneID(account.ID).Exec(ctx))
	})
	cache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, integrationDB, cache)
	before, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	countEvents := func() int {
		var count int
		require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", account.ID).Scan(&count))
		return count
	}
	baseline := countEvents()
	type outcome struct {
		changed bool
		err     error
	}
	const requests = 16
	results := make(chan outcome, requests)
	start := make(chan struct{})
	for range requests {
		go func() {
			<-start
			changed, err := repo.DisableExcelBPSOn403(ctx, before)
			results <- outcome{changed, err}
		}()
	}
	close(start)
	changedCount := 0
	for range requests {
		result := <-results
		require.NoError(t, result.err)
		if result.changed {
			changedCount++
		}
	}
	require.Equal(t, 1, changedCount)
	require.Equal(t, baseline+1, countEvents())
	after, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.False(t, after.IsExcelBPSEnabled())
	require.Len(t, cache.setAccounts, 1)
	require.False(t, cache.setAccounts[0].IsExcelBPSEnabled())
	require.True(t, before.IsExcelBPSEnabled(), "shared request snapshot must remain unchanged")
	require.Equal(t, before.Credentials, after.Credentials)
	require.Equal(t, before.Status, after.Status)
	require.Equal(t, before.Schedulable, after.Schedulable)
	require.Equal(t, before.Concurrency, after.Concurrency)
	for key, value := range before.Extra {
		if key != "openai_excel_bps" {
			require.Equal(t, value, after.Extra[key], key)
		}
	}
}
