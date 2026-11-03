//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthCredentialFailureDoesNotRetryCancellationOrUnclassifiedErrors(t *testing.T) {
	account, _, _, _ := newOpenAIRecoveryFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
	}{
		{"canceled", ctx, &openai.RefreshTokenError{Permanent: true}},
		{"transient", context.Background(), &openai.RefreshTokenError{StatusCode: http.StatusServiceUnavailable}},
		{"unclassified", context.Background(), errors.New("credential store unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			err := openAICredentialFailoverError(tc.ctx, c, account, tc.err)
			var failure *UpstreamFailoverError
			require.False(t, errors.As(err, &failure))
			if tc.ctx.Err() != nil {
				require.ErrorIs(t, err, tc.ctx.Err())
			} else {
				require.ErrorIs(t, err, tc.err)
			}
			_, hasEvents := c.Get(OpsUpstreamErrorsKey)
			require.False(t, hasEvents)
		})
	}
}

func TestOpenAIOAuthCredentialFailureEntersFailoverAndRecordsCause(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	account.Credentials["access_token"] = codexTestJWTExpiry(time.Now().Add(-time.Minute).Unix())
	repo.account = snapshotOAuthRefreshAccount(account)
	executor.refresh = func(context.Context, *Account) (map[string]any, error) {
		return nil, openai.NewRefreshTokenError(http.StatusBadRequest, []byte(`{"error":{"code":"refresh_token_reused","message":"secret-must-not-leak"}}`))
	}
	svc := &OpenAIGatewayService{openAITokenProvider: provider}
	for range 2 {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		token, _, err := svc.GetRequestCredential(context.Background(), c, account)
		require.Empty(t, token)
		var failure *UpstreamFailoverError
		require.ErrorAs(t, err, &failure)
		require.True(t, failure.IsCredentialFailure())
		require.True(t, failure.ShouldRetryNextAccount())
		require.False(t, failure.RetryableOnSameAccount)
		require.Equal(t, GatewayFailureScopeAccount, failure.Scope)
		require.EqualValues(t, "openai_oauth_reauth_required", failure.Reason)
		require.Equal(t, http.StatusServiceUnavailable, failure.ClientStatusCode)
		require.Empty(t, failure.ResponseBody)
		require.False(t, c.Writer.Written(), "the handler must still be able to try another account")
		events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
		require.Len(t, events, 1)
		require.Equal(t, "account_auth", events[0].Stage)
		require.Equal(t, "credential_failover", events[0].Kind)
		require.Equal(t, account.ID, events[0].AccountID)
		require.Nil(t, events[0].ProxyID)
		require.Equal(t, opsProxyNameUnknown, events[0].ProxyName)
		require.Contains(t, events[0].Message, "sign in again")
		require.NotContains(t, events[0].Message, "secret")
	}
	require.Equal(t, int32(1), executor.calls.Load(), "do not refresh a known rejected credential again")
	require.Zero(t, repo.setError, "a request must not disable a concurrently reauthorized account")
	require.Equal(t, account.Credentials, repo.account.Credentials)
}

func TestOpenAIOAuthCredentialFailureSchedulerSkipsOnlyRejectedVersion(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "advanced"}[advanced], func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
			account, _, _, provider := newOpenAIRecoveryFixture()
			account.Extra = astraHealthyTestExtra(nil)
			provider.refreshAPI.recordOpenAIRefreshFailure(account, &openai.RefreshTokenError{Permanent: true})
			healthy := snapshotOAuthRefreshAccount(account)
			healthy.ID++
			repo := schedulerTestOpenAIAccountRepo{accounts: []Account{*account, *healthy}}
			cfg := &config.Config{}
			cfg.Gateway.Scheduling.LoadBatchEnabled = true
			svc := &OpenAIGatewayService{
				accountRepo: repo, openAITokenProvider: provider, cfg: cfg,
				cache:              &schedulerTestGatewayCache{},
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			if advanced {
				settings := &openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{openAIAdvancedSchedulerSettingKey: "true"}}
				svc.rateLimitService = &RateLimitService{settingService: NewSettingService(settings, cfg)}
			}
			require.Equal(t, advanced, svc.isOpenAIAdvancedSchedulerEnabled(context.Background()))
			require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra", false))
			groupID := int64(10106)
			selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.Equal(t, healthy.ID, selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			reauthorized := snapshotOAuthRefreshAccount(account)
			reauthorized.Credentials["access_token"] = "reauthorized-access"
			reauthorized.Credentials["refresh_token"] = "reauthorized-refresh"
			require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(reauthorized, "gpt-6-astra", false))
			require.True(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra", false), "stale snapshots remain ineligible")
			repo.accounts[0] = *reauthorized
			selection, _, err = svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "", "gpt-6-astra", map[int64]struct{}{healthy.ID: {}}, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.Equal(t, reauthorized.ID, selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

func TestOpenAIOAuthCredentialFailureForwardPreservesFailover(t *testing.T) {
	account, _, _, provider := newOpenAIRecoveryFixture()
	account.Extra = astraHealthyTestExtra(nil)
	provider.refreshAPI.recordOpenAIRefreshFailure(account, &openai.RefreshTokenError{Permanent: true})
	upstream := &httpUpstreamRecorder{}
	svc := newOpenAIRejectedFieldTestService(upstream)
	svc.openAITokenProvider = provider
	body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`)
	c := newOpenAIRejectedFieldTestContext(body)
	_, err := svc.Forward(context.Background(), c, account, body)
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.EqualValues(t, "openai_oauth_reauth_required", failure.Reason)
	require.True(t, failure.ShouldRetryNextAccount())
	require.False(t, c.Writer.Written())
	require.Empty(t, upstream.bodies, "credential failures happen before inference")
}
