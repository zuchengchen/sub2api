//go:build unit

package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Existing OAuth fixtures implement the new mandatory atomic persistence contract.
func (r *openAIRecoveryRepo) UpdateOpenAIOAuthCredentialsIfUnchanged(_ context.Context, id int64, expected map[string]any, proxy *int64, credentials map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return false, r.updateErr
	}
	if !openAIRefreshTestStateMatches(r.account, id, expected, proxy) {
		return false, nil
	}
	r.account = snapshotOAuthRefreshAccount(r.account)
	r.account.Credentials = shallowCopyMap(credentials)
	return true, nil
}

func openAIRefreshTestStateMatches(account *Account, id int64, expected map[string]any, proxy *int64) bool {
	return account != nil && account.ID == id && account.IsOpenAIOAuth() && reflect.DeepEqual(account.Credentials, expected) && reflect.DeepEqual(account.ProxyID, proxy)
}

func (r *refreshAPIAccountRepo) UpdateOpenAIOAuthCredentialsIfUnchanged(_ context.Context, id int64, expected map[string]any, proxy *int64, credentials map[string]any) (bool, error) {
	r.successCASCalls++
	r.lastExpectedCredentials = shallowCopyMap(expected)
	r.lastExpectedProxyID = proxy
	if r.beforeSuccessCAS != nil {
		r.beforeSuccessCAS(r)
	}
	if r.updateErr != nil {
		return false, r.updateErr
	}
	if !openAIRefreshTestStateMatches(r.account, id, expected, proxy) {
		return false, nil
	}
	r.updateCalls++
	r.updateCredentialsCalls++
	r.account = snapshotOAuthRefreshAccount(r.account)
	r.account.Credentials = shallowCopyMap(credentials)
	return true, nil
}

func (r *tokenRefreshAccountRepo) UpdateOpenAIOAuthCredentialsIfUnchanged(_ context.Context, id int64, expected map[string]any, proxy *int64, credentials map[string]any) (bool, error) {
	r.conditionalSuccessCalls++
	if r.conditionalSuccessErr != nil {
		return false, r.conditionalSuccessErr
	}
	if r.updateErr != nil {
		return false, r.updateErr
	}
	account := r.accountsByID[id]
	if !openAIRefreshTestStateMatches(account, id, expected, proxy) {
		return false, nil
	}
	r.updateCalls++
	r.updateCredentialsCalls++
	account.Credentials = shallowCopyMap(credentials)
	r.lastAccount = account
	if r.cancelOnUpdate != nil {
		r.cancelOnUpdate()
	}
	return true, nil
}

type openAIRefreshCASRepo struct {
	openAIRecoveryRepo
	casCalls            int
	beforeCAS           func(*Account)
	readFailureAfterCAS bool
}

func (r *openAIRefreshCASRepo) UpdateOpenAIOAuthCredentialsIfUnchanged(_ context.Context, id int64, expected map[string]any, proxy *int64, credentials map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.casCalls++
	if r.beforeCAS != nil {
		r.beforeCAS(r.account)
	}
	if r.updateErr != nil {
		return false, r.updateErr
	}
	applied := openAIRefreshTestStateMatches(r.account, id, expected, proxy)
	if applied {
		r.account = snapshotOAuthRefreshAccount(r.account)
		r.account.Credentials = shallowCopyMap(credentials)
	}
	if r.readFailureAfterCAS {
		r.readErr = errors.New("synthetic durable read failure")
	}
	return applied, nil
}

func newOpenAIRefreshCASFixture() (*Account, *openAIRefreshCASRepo, *openAIRecoveryExecutor, *OAuthRefreshAPI, *refreshAPICacheStub) {
	account, repo, executor, _ := newOpenAIRecoveryFixture()
	wrapped := &openAIRefreshCASRepo{openAIRecoveryRepo: openAIRecoveryRepo{account: snapshotOAuthRefreshAccount(repo.account)}}
	cache := &refreshAPICacheStub{lockResult: true}
	return account, wrapped, executor, NewOAuthRefreshAPI(wrapped, cache), cache
}
func forceOpenAIRefreshForTest(executor OAuthRefreshExecutor, account *Account) OAuthRefreshExecutor {
	return &openAIManualRefreshExecutor{OAuthRefreshExecutor: executor, expected: snapshotOAuthRefreshAccount(account)}
}

func TestOpenAIRefreshSuccessCAS_ConcurrentCredentialOrProxyEditWins(t *testing.T) {
	for _, mode := range []string{"other_user", "same_user_new_credentials", "credential_setting", "proxy", "auth_mode"} {
		t.Run(mode, func(t *testing.T) {
			account, repo, executor, api, cache := newOpenAIRefreshCASFixture()
			repo.beforeCAS = func(current *Account) {
				switch mode {
				case "other_user":
					current.Credentials["chatgpt_user_id"] = "replacement-user"
				case "same_user_new_credentials":
					current.Credentials["access_token"] = "admin-access"
					current.Credentials["refresh_token"] = "admin-refresh"
				case "credential_setting":
					current.Credentials["operator_setting"] = "updated"
				case "proxy":
					proxy := int64(29)
					current.ProxyID = &proxy
				case "auth_mode":
					current.Credentials["openai_auth_mode"] = "agent_identity"
				}
			}
			result, err := api.RefreshIfNeeded(context.Background(), account, forceOpenAIRefreshForTest(executor, account), 0)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.Refreshed)
			require.Nil(t, result.NewCredentials)
			require.Equal(t, int32(1), executor.calls.Load())
			require.Equal(t, 1, repo.casCalls)
			require.NotEqual(t, "rotated-refresh", result.Account.GetOpenAIRefreshToken())
			require.Zero(t, executor.published.Load())
			require.Zero(t, cache.deleteCalls)
			require.Equal(t, "old-refresh", account.GetOpenAIRefreshToken(), "attempt snapshot must remain immutable")
		})
	}
}

func TestOpenAIRefreshSuccessCAS_NormalRefreshUsesDurableSchedulingState(t *testing.T) {
	account, repo, executor, api, cache := newOpenAIRefreshCASFixture()
	repo.beforeCAS = func(current *Account) { current.Status = StatusDisabled; current.Schedulable = false }
	result, err := api.RefreshIfNeeded(context.Background(), account, forceOpenAIRefreshForTest(executor, account), 0)
	require.NoError(t, err)
	require.True(t, result.Refreshed)
	require.Equal(t, "rotated-refresh", result.Account.GetOpenAIRefreshToken())
	require.Equal(t, StatusDisabled, result.Account.Status)
	require.False(t, result.Account.Schedulable)
	require.Equal(t, int32(1), executor.published.Load())
	require.Equal(t, 1, cache.deleteCalls)
	require.Equal(t, OpenAITokenCacheKey(account), cache.deleteKey)
	require.Equal(t, "old-refresh", account.GetOpenAIRefreshToken())
}

type openAIRefreshNoCASRepo struct {
	AccountRepository
	account *Account
}

func (r *openAIRefreshNoCASRepo) GetByID(context.Context, int64) (*Account, error) {
	return snapshotOAuthRefreshAccount(r.account), nil
}

func TestOpenAIRefreshSuccessCAS_RequiresPersistenceContractBeforeNetwork(t *testing.T) {
	account, _, executor, _, _ := newOpenAIRefreshCASFixture()
	api := NewOAuthRefreshAPI(&openAIRefreshNoCASRepo{account: account}, nil)
	result, err := api.RefreshIfNeeded(context.Background(), account, forceOpenAIRefreshForTest(executor, account), 0)
	require.Nil(t, result)
	require.Error(t, err)
	require.True(t, isProviderScopedTerminalRefreshError(err))
	var unavailable *oauthRefreshStateUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Zero(t, executor.calls.Load())
}

func TestOpenAIRefreshSuccessCAS_PersistenceFailuresNeverReturnUncommittedToken(t *testing.T) {
	for _, mode := range []string{"write_error", "cas_miss_read_error", "applied_read_error"} {
		t.Run(mode, func(t *testing.T) {
			account, repo, executor, api, cache := newOpenAIRefreshCASFixture()
			if mode == "write_error" {
				repo.updateErr = errors.New("synthetic atomic outbox failure")
			} else {
				repo.readFailureAfterCAS = true
			}
			if mode == "cas_miss_read_error" {
				repo.beforeCAS = func(current *Account) { current.Credentials["refresh_token"] = "admin-refresh" }
			}
			result, err := api.RefreshIfNeeded(context.Background(), account, forceOpenAIRefreshForTest(executor, account), 0)
			require.Nil(t, result)
			require.Error(t, err)
			require.True(t, isProviderScopedTerminalRefreshError(err))
			var unavailable *oauthRefreshStateUnavailableError
			require.ErrorAs(t, err, &unavailable)
			require.Equal(t, int32(1), executor.calls.Load())
			if mode == "applied_read_error" {
				require.Equal(t, 1, cache.deleteCalls)
				require.Equal(t, int32(1), executor.published.Load())
			} else {
				require.Zero(t, cache.deleteCalls)
				require.Zero(t, executor.published.Load())
			}
		})
	}
}

func TestOpenAIRefreshSuccessCAS_BackgroundPersistenceFailureIsNotRetried(t *testing.T) {
	account, repo, executor, api, _ := newOpenAIRefreshCASFixture()
	repo.updateErr = errors.New("synthetic write error")
	cfg := &config.Config{TokenRefresh: config.TokenRefreshConfig{MaxRetries: 3}}
	svc := NewTokenRefreshService(repo, nil, nil, nil, nil, cfg, nil)
	svc.SetRefreshAPI(api)
	forced := forceOpenAIRefreshForTest(executor, account)
	err := svc.refreshWithRetry(context.Background(), account, forced, forced, 0)
	require.Error(t, err)
	require.True(t, isProviderScopedTerminalRefreshError(err))
	require.Equal(t, int32(1), executor.calls.Load())
	require.Zero(t, repo.setError)
}

func TestOpenAIRefreshSuccessCAS_BackgroundRequiresSharedCoordinator(t *testing.T) {
	account, repo, executor, _, _ := newOpenAIRefreshCASFixture()
	svc := NewTokenRefreshService(repo, nil, nil, nil, nil, &config.Config{}, nil)
	err := svc.refreshWithRetry(context.Background(), account, executor, nil, 0)
	require.Error(t, err)
	require.True(t, isProviderScopedTerminalRefreshError(err))
	require.Zero(t, executor.calls.Load())
}

func TestOpenAIRefreshSuccessCAS_AdminAndRequestShareOneRotation(t *testing.T) {
	account, repo, executor, api, _ := newOpenAIRefreshCASFixture()
	// The cache stub is deliberately not used concurrently; the shared local lock
	// and durable credential reread are the behavior under test here.
	api.tokenCache = nil
	svc := &OpenAIOAuthService{}
	svc.SetStoredAccountRefresh(api, executor)
	entered := make(chan struct{})
	release := make(chan struct{})
	executor.refresh = func(_ context.Context, current *Account) (map[string]any, error) {
		close(entered)
		<-release
		credentials := shallowCopyMap(current.Credentials)
		credentials["access_token"] = "rotated-access"
		credentials["refresh_token"] = "rotated-refresh"
		return credentials, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	manual := make(chan error, 1)
	go func() {
		updated, err := svc.RefreshStoredAccount(ctx, account)
		if err == nil && updated.GetOpenAIAccessToken() != "rotated-access" {
			err = errors.New("wrong manual token")
		}
		manual <- err
	}()
	<-entered
	provider := NewOpenAITokenProvider(repo, nil, nil)
	provider.SetRefreshAPI(api, executor)
	request := make(chan error, 1)
	go func() {
		token, err := provider.RefreshAfterUnauthorized(ctx, account, account.GetOpenAIAccessToken())
		if err == nil && token != "rotated-access" {
			err = errors.New("wrong request token")
		}
		request <- err
	}()
	close(release)
	require.NoError(t, <-manual)
	require.NoError(t, <-request)
	require.Equal(t, int32(1), executor.calls.Load())
	require.Equal(t, 1, repo.casCalls)
}

func TestOpenAIRefreshSuccessCAS_ManualAllowsInactiveOrAccessTokenOnlyAccount(t *testing.T) {
	for _, mode := range []string{"inactive", "access_token_only"} {
		t.Run(mode, func(t *testing.T) {
			account, repo, executor, api, _ := newOpenAIRefreshCASFixture()
			if mode == "inactive" {
				account.Status = StatusError
			} else {
				delete(account.Credentials, "refresh_token")
			}
			repo.account = snapshotOAuthRefreshAccount(account)
			svc := &OpenAIOAuthService{}
			svc.SetStoredAccountRefresh(api, executor)
			updated, err := svc.RefreshStoredAccount(context.Background(), account)
			require.NoError(t, err)
			require.NotNil(t, updated)
			require.Equal(t, int32(1), executor.calls.Load())
			require.Equal(t, account.Status, updated.Status)
		})
	}
}

func TestOpenAIRefreshSuccessCAS_TransactionDefersPublicationsAndUnlock(t *testing.T) {
	for _, mode := range []string{"commit", "rollback", "commit_failure"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			mock.ExpectBegin()
			tx, err := client.Tx(context.Background())
			require.NoError(t, err)
			account, _, executor, api, cache := newOpenAIRefreshCASFixture()
			ctx := dbent.NewTxContext(context.Background(), tx)
			result, err := api.RefreshIfNeeded(ctx, account, forceOpenAIRefreshForTest(executor, account), 0)
			require.NoError(t, err)
			require.True(t, result.Refreshed)
			require.Zero(t, executor.published.Load())
			require.Zero(t, cache.deleteCalls)
			require.Zero(t, cache.releaseCalls)
			lock := api.getLocalLock(OpenAITokenCacheKey(account))
			waiting, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
			require.ErrorIs(t, lock.Lock(waiting), context.DeadlineExceeded)
			stop()
			switch mode {
			case "commit":
				mock.ExpectCommit()
				require.NoError(t, tx.Commit())
				require.Equal(t, int32(1), executor.published.Load())
				require.Equal(t, 1, cache.deleteCalls)
			case "rollback":
				mock.ExpectRollback()
				require.NoError(t, tx.Rollback())
				require.Zero(t, executor.published.Load())
				require.Zero(t, cache.deleteCalls)
			case "commit_failure":
				mock.ExpectCommit().WillReturnError(errors.New("synthetic commit failure"))
				require.Error(t, tx.Commit())
				require.Zero(t, executor.published.Load())
				require.Zero(t, cache.deleteCalls)
			}
			require.Equal(t, 1, cache.releaseCalls)
			waiting, stop = context.WithTimeout(context.Background(), time.Second)
			defer stop()
			require.NoError(t, lock.Lock(waiting))
			lock.Unlock()
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func configureOpenAIRefreshTestCoordinator(service *TokenRefreshService, repo *tokenRefreshAccountRepo, account *Account) {
	if account.IsOpenAIOAuth() {
		account.Status = StatusActive
		if account.Credentials == nil {
			account.Credentials = map[string]any{}
		}
		repo.accountsByID = map[int64]*Account{account.ID: account}
		service.SetRefreshAPI(NewOAuthRefreshAPI(repo, nil))
	}
}

func TestOpenAIRefreshSuccessCAS_BackgroundRateWrapperPublishesAfterCAS(t *testing.T) {
	account, _, executor, api, _ := newOpenAIRefreshCASFixture()
	wrapped := &rateLimitedOAuthRefreshExecutor{OAuthRefreshExecutor: forceOpenAIRefreshForTest(executor, account), acquireRate: func(context.Context) (func(), error) { return func() {}, nil }}
	result, err := api.RefreshIfNeeded(context.Background(), account, wrapped, 0)
	require.NoError(t, err)
	require.True(t, result.Refreshed)
	require.Equal(t, int32(1), executor.published.Load())
}

func TestOpenAIRefreshSuccessCAS_CRSPostImportUsesCoordinator(t *testing.T) {
	account, repo, executor, api, _ := newOpenAIRefreshCASFixture()
	repo.beforeCAS = func(current *Account) { current.Credentials["refresh_token"] = "admin-replacement" }
	oauth := &OpenAIOAuthService{}
	oauth.SetStoredAccountRefresh(api, executor)
	svc := &CRSSyncService{openaiOAuthService: oauth, accountRepo: repo}
	svc.refreshStoredOpenAIAccount(context.Background(), account)
	require.Equal(t, int32(1), executor.calls.Load())
	require.Equal(t, 1, repo.casCalls)
	require.Equal(t, "admin-replacement", repo.account.GetOpenAIRefreshToken())
	require.Zero(t, executor.published.Load())
}
