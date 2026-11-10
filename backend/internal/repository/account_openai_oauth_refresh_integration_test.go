//go:build integration

package repository

import (
	"context"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func (s *AccountRepoSuite) TestOpenAIOAuthSuccessCAS_RefreshOrConcurrentEdit() {
	for _, mode := range []string{"success", "same_owner_reauthorization", "different_owner", "proxy"} {
		s.Run(mode, func() {
			account := mustCreateAccount(s.T(), s.client, &service.Account{Name: "openai-success-cas-" + mode, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "observed", "refresh_token": "observed-refresh", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}})
			observed, err := s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(err)
			switch mode {
			case "same_owner_reauthorization", "different_owner":
				current := map[string]any{"access_token": "admin-access", "refresh_token": "admin-refresh", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}
				if mode == "different_owner" {
					current["chatgpt_user_id"] = "other-user"
				}
				s.Require().NoError(s.repo.UpdateCredentials(s.ctx, account.ID, current))
			case "proxy":
				proxy, createErr := s.client.Proxy.Create().SetName("openai-cas-proxy").SetProtocol("http").SetHost("127.0.0.1").SetPort(12345).SetStatus(service.StatusActive).Save(s.ctx)
				s.Require().NoError(createErr)
				_, err = s.repo.sql.ExecContext(s.ctx, "UPDATE accounts SET proxy_id = $1 WHERE id = $2", proxy.ID, account.ID)
				s.Require().NoError(err)
			}
			cache := &schedulerCacheRecorder{}
			s.repo.schedulerCache = cache
			_, err = s.repo.sql.ExecContext(s.ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
			s.Require().NoError(err)
			applied, err := s.repo.UpdateOpenAIOAuthCredentialsIfUnchanged(s.ctx, account.ID, observed.Credentials, observed.ProxyID, map[string]any{"access_token": "rotated", "refresh_token": "rotated-refresh", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user"})
			s.Require().NoError(err)
			s.Require().Equal(mode == "success", applied)
			got, err := s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(err)
			var count int
			err = scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox WHERE event_type = $1 AND account_id = $2", []any{service.SchedulerOutboxEventAccountChanged, account.ID}, &count)
			s.Require().NoError(err)
			if mode == "success" {
				s.Require().Equal("rotated-refresh", got.GetOpenAIRefreshToken())
				s.Require().Len(cache.setAccounts, 1)
				s.Require().Equal(1, count)
			} else {
				s.Require().NotEqual("rotated-refresh", got.GetOpenAIRefreshToken())
				s.Require().Empty(cache.setAccounts)
				s.Require().Zero(count)
			}
		})
	}
}

func TestAccountRepoOpenAIOAuthSuccessCAS_OuterTransactionPublication(t *testing.T) {
	client := testEntClient(t)
	parentCtx := context.Background()
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "commit"}[commit], func(t *testing.T) {
			repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
			account := mustCreateAccount(t, client, &service.Account{Name: "openai-transaction-cas", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "observed"}})
			t.Cleanup(func() {
				_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
				_ = client.Account.DeleteOneID(account.ID).Exec(context.Background())
			})
			observed, err := repo.GetByID(parentCtx, account.ID)
			require.NoError(t, err)
			_, err = repo.sql.ExecContext(parentCtx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
			require.NoError(t, err)
			cache := &schedulerCacheRecorder{}
			repo.schedulerCache = cache
			tx, err := client.Tx(parentCtx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			ctx := dbent.NewTxContext(parentCtx, tx)
			applied, err := repo.UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, account.ID, observed.Credentials, observed.ProxyID, map[string]any{"access_token": "rotated"})
			require.NoError(t, err)
			require.True(t, applied)
			require.Empty(t, cache.setAccounts)
			if commit {
				require.NoError(t, tx.Commit())
			} else {
				require.NoError(t, tx.Rollback())
			}
			got, err := repo.GetByID(parentCtx, account.ID)
			require.NoError(t, err)
			var count int
			err = scanSingleRow(parentCtx, repo.sql, "SELECT COUNT(*) FROM scheduler_outbox WHERE event_type = $1 AND account_id = $2", []any{service.SchedulerOutboxEventAccountChanged, account.ID}, &count)
			require.NoError(t, err)
			if commit {
				require.Equal(t, "rotated", got.GetOpenAIAccessToken())
				require.Len(t, cache.setAccounts, 1)
				require.Equal(t, 1, count)
			} else {
				require.Equal(t, "observed", got.GetOpenAIAccessToken())
				require.Empty(t, cache.setAccounts)
				require.Zero(t, count)
			}
		})
	}
}

func TestOpenAIOAuthSuccessCAS_AtomicOutboxFailureRollsBackCredentials(t *testing.T) {
	client := testEntClient(t)
	account := mustCreateAccount(t, client, &service.Account{Name: "openai-cas-atomic-failure", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "observed", "refresh_token": "observed-refresh"}})
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
		_ = client.Account.DeleteOneID(account.ID).Exec(context.Background())
	})
	repo := newAccountRepositoryWithSQL(client, &failAtomicSchedulerOutboxSQLExecutor{sqlExecutor: integrationDB}, nil)
	applied, err := repo.UpdateOpenAIOAuthCredentialsIfUnchanged(context.Background(), account.ID, account.Credentials, account.ProxyID, map[string]any{"access_token": "rotated", "refresh_token": "rotated-refresh"})
	require.Error(t, err)
	require.False(t, applied)
	got, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "observed-refresh", got.GetOpenAIRefreshToken())
	var count int
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id = $1", account.ID).Scan(&count))
	require.Zero(t, count)
}
