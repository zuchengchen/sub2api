//go:build unit

package service

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestOpenAIAutomaticAccessVerificationRestoresScheduledTraffic(t *testing.T) {
	ctx := context.Background()
	account, accounts, executor, tokens := newOpenAIRecoveryFixture()
	// A real token can still serve requests during the proactive refresh window.
	account.Credentials["access_token"] = codexTestJWTExpiry(time.Now().Add(4 * time.Minute).Unix())
	account.Extra = astraHealthyTestExtra(nil)
	accounts.account = snapshotOAuthRefreshAccount(account)
	executor.refresh = func(context.Context, *Account) (map[string]any, error) {
		return nil, &openai.RefreshTokenError{StatusCode: http.StatusBadRequest, Code: "refresh_token_reused", Permanent: true}
	}

	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	gateway := &OpenAIGatewayService{
		accountRepo:         schedulerTestOpenAIAccountRepo{accounts: []Account{*account}},
		openAITokenProvider: tokens,
		cfg:                 cfg,
		cache:               &schedulerTestGatewayCache{},
		concurrencyService:  NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	proceed := make(chan struct{})
	var calls atomic.Int32
	upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		calls.Add(1)
		select {
		case <-proceed:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}]}}\n\n"
		resp := newJSONResponse(http.StatusOK, body)
		resp.Header.Set("Content-Type", "text/event-stream")
		resp.Request = req
		return resp, nil
	}}
	tests := ProvideAccountTestService(accounts, nil, nil, upstream, cfg, nil, gateway, nil, nil)
	worker := ProvideTokenRefreshService(accounts, nil, nil, nil, nil, nil, cfg, nil, nil, nil, tokens.refreshAPI, gateway, tests)
	t.Cleanup(worker.Stop)

	groupID := int64(37)
	selectAccount := func() (*AccountSelectionResult, error) {
		selection, _, err := gateway.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
		return selection, err
	}

	_, err := tokens.GetAccessToken(ctx, account)
	var rejection *openai.RefreshTokenError
	require.ErrorAs(t, err, &rejection)
	require.True(t, rejection.Permanent)
	require.Nil(t, tests.OpenAIAccessAutoVerifiedUntil(account))
	_, err = selectAccount()
	require.Error(t, err)

	close(proceed)
	require.Eventually(t, func() bool {
		return !gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6-astra", false)
	}, 5*time.Second, 5*time.Millisecond)

	selection, err := selectAccount()
	require.NoError(t, err)
	require.Equal(t, account.ID, selection.Account.ID)
	if selection.ReleaseFunc != nil {
		defer selection.ReleaseFunc()
	}
	token, err := tokens.GetAccessToken(ctx, selection.Account)
	require.NoError(t, err)
	require.Equal(t, account.GetOpenAIAccessToken(), token)
	require.Equal(t, openAIAccessTokenExpiresAt(account), tests.OpenAIAccessAutoVerifiedUntil(account))
	otherVersion := snapshotOAuthRefreshAccount(account)
	otherVersion.Credentials["refresh_token"] = "another-grant"
	require.Nil(t, tests.OpenAIAccessAutoVerifiedUntil(otherVersion))
	tokens.refreshAPI.rejectVerifiedOpenAIAccess(account, "Bearer "+token)
	require.Nil(t, tests.OpenAIAccessAutoVerifiedUntil(account))
	require.EqualValues(t, 1, executor.calls.Load(), "the rejected refresh token must never be submitted again")
	require.EqualValues(t, 1, calls.Load(), "a single completed inference is enough")
}
