//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

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

func (s *AccountRepoSuite) TestOpenAIOAuthRefreshErrorCAS_PreservesSchedulingAndRecovery() {
	for _, mode := range []string{"enabled", "admin_disabled", "reauthorized"} {
		s.Run(mode, func() {
			account := mustCreateAccount(s.T(), s.client, &service.Account{
				Name: "openai-refresh-error-" + mode, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "observed", "refresh_token": "observed-refresh"},
			})
			observed, err := s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(err)
			switch mode {
			case "admin_disabled":
				s.Require().NoError(s.repo.SetSchedulable(s.ctx, account.ID, false))
			case "reauthorized":
				s.Require().NoError(s.repo.UpdateCredentials(s.ctx, account.ID, map[string]any{"access_token": "rotated", "refresh_token": "rotated-refresh"}))
			}
			applied, err := s.repo.SetOpenAIOAuthRefreshErrorIfCredentialsUnchanged(s.ctx, account.ID, observed.Credentials, observed.ProxyID, "refresh rejected")
			s.Require().NoError(err)
			latest, err := s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(err)
			if mode == "reauthorized" {
				s.Require().False(applied)
				s.Require().True(latest.IsSchedulable())
				s.Require().Equal("rotated-refresh", latest.GetOpenAIRefreshToken())
				return
			}

			s.Require().True(applied)
			s.Require().Equal(service.StatusError, latest.Status)
			s.Require().False(latest.IsSchedulable())
			s.Require().Equal(mode == "enabled", latest.Schedulable)
			limiter := service.NewRateLimitService(s.repo, nil, nil, nil)
			result, err := limiter.RecoverAccountAfterSuccessfulTest(s.ctx, account.ID)
			s.Require().NoError(err)
			s.Require().True(result.ClearedError)
			latest, err = s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(err)
			s.Require().Equal(service.StatusActive, latest.Status)
			s.Require().Equal(mode == "enabled", latest.IsSchedulable())
		})
	}
}

func (s *AccountRepoSuite) TestOpenAIAccessProbeStartupPagesOnlyRecoverableErrors() {
	const message = "Token refresh failed (non-retryable): OpenAI OAuth credentials permanently rejected; sign in again"
	add := func(name string, mutate func(*dbent.AccountCreate)) int64 {
		create := s.client.Account.Create().SetName(name).SetPlatform(service.PlatformOpenAI).
			SetType(service.AccountTypeOAuth).SetStatus(service.StatusError).
			SetSchedulable(true).SetErrorMessage(message)
		if mutate != nil {
			mutate(create)
		}
		row, err := create.Save(s.ctx)
		s.Require().NoError(err)
		return row.ID
	}

	cursor := add("before-cursor", nil)
	first := add("first-matching", nil)
	add("manual-stop", func(b *dbent.AccountCreate) { b.SetSchedulable(false) })
	add("other-failure", func(b *dbent.AccountCreate) { b.SetErrorMessage("operator isolation") })
	add("active", func(b *dbent.AccountCreate) { b.SetStatus(service.StatusActive) })
	add("other-platform", func(b *dbent.AccountCreate) { b.SetPlatform(service.PlatformAnthropic) })
	add("api-key", func(b *dbent.AccountCreate) { b.SetType(service.AccountTypeAPIKey) })
	add("deleted", func(b *dbent.AccountCreate) { b.SetDeletedAt(time.Now()) })
	second := add("second-matching", nil)
	for _, expected := range []int64{first, second} {
		page, err := s.repo.ListOpenAIOAuthRefreshErrors(s.ctx, cursor, 1, message)
		s.Require().NoError(err)
		s.Require().Len(page, 1)
		s.Require().Equal(expected, page[0].ID)
		cursor = page[0].ID
	}
	page, err := s.repo.ListOpenAIOAuthRefreshErrors(s.ctx, cursor, 1, message)
	s.Require().NoError(err)
	s.Require().Empty(page)
}

func (s *AccountRepoSuite) TestOpenAIAccessProbeCAS_RecoversOnlyMatchingRefreshError() {
	const refreshMessage = "Token refresh failed (non-retryable): OpenAI OAuth credentials permanently rejected; sign in again"
	for _, mode := range []string{"matching", "new_credentials", "new_proxy", "new_error", "manually_stopped", "already_active"} {
		s.Run(mode, func() {
			account := mustCreateAccount(s.T(), s.client, &service.Account{
				Name: "openai-access-probe-" + mode, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "observed", "refresh_token": "observed-refresh"},
				Status:      service.StatusError, ErrorMessage: refreshMessage, Schedulable: true,
			})
			observed, err := s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(err)
			// Persist a separate cooldown that a credential check cannot clear.
			_, err = s.repo.sql.ExecContext(s.ctx, "UPDATE accounts SET rate_limit_reset_at = now() + interval '1 hour' WHERE id = $1", account.ID)
			s.Require().NoError(err)
			switch mode {
			case "new_credentials":
				s.Require().NoError(s.repo.UpdateCredentials(s.ctx, account.ID, map[string]any{"access_token": "rotated", "refresh_token": "new-refresh"}))
			case "new_proxy":
				proxy, proxyErr := s.client.Proxy.Create().SetName("openai-access-probe-proxy").SetProtocol("http").SetHost("127.0.0.1").SetPort(12345).SetStatus(service.StatusActive).Save(s.ctx)
				s.Require().NoError(proxyErr)
				_, err = s.repo.sql.ExecContext(s.ctx, "UPDATE accounts SET proxy_id = $1 WHERE id = $2", proxy.ID, account.ID)
				s.Require().NoError(err)
			case "new_error":
				_, err = s.repo.sql.ExecContext(s.ctx, "UPDATE accounts SET error_message = 'operator isolation' WHERE id = $1", account.ID)
				s.Require().NoError(err)
			case "manually_stopped":
				s.Require().NoError(s.repo.SetSchedulable(s.ctx, account.ID, false))
			case "already_active":
				s.Require().NoError(s.repo.ClearError(s.ctx, account.ID))
			}
			before, err := s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(err)
			_, err = s.repo.sql.ExecContext(s.ctx, "DELETE FROM scheduler_outbox WHERE account_id = $1", account.ID)
			s.Require().NoError(err)
			applied, err := s.repo.ClearOpenAIOAuthRefreshErrorIfCredentialsUnchanged(s.ctx, account.ID, observed.Credentials, observed.ProxyID, refreshMessage)
			s.Require().NoError(err)
			s.Require().Equal(mode == "matching", applied)
			after, err := s.repo.GetByID(s.ctx, account.ID)
			s.Require().NoError(err)
			s.Require().Equal(before.Schedulable, after.Schedulable)
			s.Require().Equal(before.Credentials, after.Credentials)
			s.Require().Equal(before.RateLimitResetAt, after.RateLimitResetAt)
			var events int
			err = scanSingleRow(s.ctx, s.repo.sql, "SELECT COUNT(*) FROM scheduler_outbox WHERE event_type = $1 AND account_id = $2", []any{service.SchedulerOutboxEventAccountChanged, account.ID}, &events)
			s.Require().NoError(err)
			if applied {
				s.Require().Equal(service.StatusActive, after.Status)
				s.Require().Empty(after.ErrorMessage)
				s.Require().Equal(1, events)
			} else {
				s.Require().Equal(before.Status, after.Status)
				s.Require().Equal(before.ErrorMessage, after.ErrorMessage)
				s.Require().Zero(events)
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
