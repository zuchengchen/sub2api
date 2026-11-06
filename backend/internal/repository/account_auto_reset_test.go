package repository

import (
	"context"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestLockAndMergeAccountExtraPreservesLatestAutoResetAttempt(t *testing.T) {
	for _, current := range []string{
		`{"codex_auto_reset_credit_state":{"status":"resetting","attempt_credit_hash":"latest"}}`,
		`{}`,
	} {
		t.Run(current, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			mock.ExpectQuery(`(?s)SELECT.*FOR NO KEY UPDATE`).
				WithArgs(int64(41), service.PlatformOpenAI, service.AccountTypeOAuth, `{"access_token":"test"}`, nil).
				WillReturnRows(sqlmock.NewRows(accountProbeExtraMockColumns()).
					AddRow(true, false, true, nil, nil, nil, nil, nil, nil, false, nil, nil, []byte(current)))
			account := &service.Account{
				ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "test"},
				Extra: map[string]any{
					service.OpenAIAutoResetCreditEnabledExtraKey: false,
					service.OpenAIAutoResetCreditStateExtraKey:   map[string]any{"status": "success", "attempt_credit_hash": "stale"},
				},
			}
			extra, err := lockAndMergeAccountProbeExtra(context.Background(), client, account, nil, nil)
			require.NoError(t, err)
			require.Equal(t, false, extra[service.OpenAIAutoResetCreditEnabledExtraKey])
			if current == `{}` {
				require.NotContains(t, extra, service.OpenAIAutoResetCreditStateExtraKey)
			} else {
				require.Equal(t, map[string]any{"status": "resetting", "attempt_credit_hash": "latest"}, extra[service.OpenAIAutoResetCreditStateExtraKey])
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestSchedulerProjectionPreservesAutoResetConfigurationAndState(t *testing.T) {
	account := service.Account{
		ID: 41, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{
			service.OpenAIAutoResetCreditEnabledExtraKey:      true,
			service.OpenAIAutoResetCreditOptInVersionExtraKey: 1,
			service.OpenAIAutoResetCredit5hThresholdExtraKey:  0.85,
			service.OpenAIAutoResetCredit7dThresholdExtraKey:  0.95,
			service.OpenAIAutoResetCreditStateExtraKey:        map[string]any{"status": "available", "available_count": 1},
		},
	}
	projected := buildSchedulerMetadataAccount(account)
	require.Equal(t, service.ResolveOpenAIAutoResetCreditConfig(&account), service.ResolveOpenAIAutoResetCreditConfig(&projected))
	require.Equal(t, account.Extra[service.OpenAIAutoResetCreditStateExtraKey], projected.Extra[service.OpenAIAutoResetCreditStateExtraKey])
	require.False(t, shouldEnqueueSchedulerOutboxForExtraUpdates(map[string]any{
		service.OpenAIAutoResetCreditStateExtraKey: map[string]any{"status": "resetting"},
	}), "runtime status updates should only refresh account snapshots")
	require.True(t, shouldEnqueueSchedulerOutboxForExtraUpdates(map[string]any{
		service.OpenAIAutoResetCreditEnabledExtraKey: true,
	}))
}
