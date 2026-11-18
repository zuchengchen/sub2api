//go:build unit

package service

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type closeOnAccountTestComplete struct {
	gin.ResponseWriter
	cancel context.CancelFunc
}

func (w closeOnAccountTestComplete) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if bytes.Contains(data, []byte(`"type":"test_complete"`)) {
		w.cancel()
	}
	return n, err
}

func TestAccountTestService_OpenAIVerifiesAccessAfterRejectedRefresh(t *testing.T) {
	const completed = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	for _, tc := range []struct {
		name          string
		body          string
		changeRequest func(*http.Request)
		newRejection  bool
		readOnly      bool
		cancel        bool
		wantTestError bool
		wantAccess    bool
	}{
		{name: "client closes on successful test_complete", body: completed, wantAccess: true},
		{name: "failed SSE", body: "data: {\"type\":\"response.failed\"}\n\n", wantTestError: true},
		{name: "EOF after content", body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n", wantTestError: true},
		{name: "canceled request", body: completed, cancel: true},
		{name: "newer rejection while testing", body: completed, newRejection: true},
		{name: "read-only intelligent test", body: completed, readOnly: true},
		{name: "different final bearer", body: completed, changeRequest: func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") }},
		{name: "different final workspace", body: completed, changeRequest: func(r *http.Request) { r.Header.Set("chatgpt-account-id", "other") }},
		{name: "redirect", body: completed, changeRequest: func(r *http.Request) { r.URL.Path = "/other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			api := provider.refreshAPI
			refreshErr := &openai.RefreshTokenError{Permanent: true, Code: "refresh_token_reused"}
			api.recordOpenAIRefreshFailure(account, refreshErr)
			gateway := &OpenAIGatewayService{openAITokenProvider: provider}
			require.True(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6.2-astra", false))

			ctx, recorder := newTestContext()
			runCtx, cancel := context.WithCancel(ctx.Request.Context())
			defer cancel()
			if tc.readOnly {
				runCtx = context.WithValue(runCtx, intelligentRunKey{}, &intelligentRunContext{})
			}
			ctx.Request = ctx.Request.WithContext(runCtx)
			if tc.wantAccess {
				ctx.Writer = closeOnAccountTestComplete{ctx.Writer, cancel}
			}
			upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, id int64, _ int) (*http.Response, error) {
				require.Equal(t, account.ID, id)
				require.Equal(t, "Bearer "+account.GetOpenAIAccessToken(), req.Header.Get("Authorization"))
				require.Equal(t, account.GetChatGPTAccountID(), req.Header.Get("chatgpt-account-id"))
				resp := newJSONResponse(http.StatusOK, tc.body)
				resp.Request = req.Clone(req.Context())
				if tc.changeRequest != nil {
					tc.changeRequest(resp.Request)
				}
				if tc.newRejection {
					api.recordOpenAIRefreshFailure(account, refreshErr)
				}
				if tc.cancel {
					cancel()
				}
				return resp, nil
			}}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, openaiGatewayService: gateway}
			db := &rateLimitClearRepoStub{getByIDAccount: &Account{ID: account.ID, Status: StatusError}}
			limiter := NewRateLimitService(db, nil, nil, nil)
			var decisions []bool
			opts := AccountTestOptions{OnOpenAIAccessVerification: func(recoveryCtx context.Context, verified bool) {
				decisions = append(decisions, verified)
				if verified {
					require.NoError(t, recoveryCtx.Err())
					_, bounded := recoveryCtx.Deadline()
					require.True(t, bounded)
					_, err := limiter.RecoverAccountAfterSuccessfulTest(recoveryCtx, account.ID)
					require.NoError(t, err)
				}
			}}
			err := svc.TestAccountConnection(ctx, account.ID, "gpt-6.2-astra", "", AccountTestModeDefault, opts)
			if tc.wantTestError {
				require.Error(t, err)
				require.NotContains(t, recorder.Body.String(), `"success":true`)
			} else {
				require.NoError(t, err)
				require.Contains(t, recorder.Body.String(), `"success":true`)
			}
			if tc.wantTestError || tc.readOnly {
				require.Empty(t, decisions)
			} else {
				require.Equal(t, []bool{tc.wantAccess}, decisions)
			}
			require.Equal(t, !tc.wantAccess, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6.2-astra", false))
			token, err := provider.GetAccessToken(context.Background(), account)
			if tc.wantAccess {
				require.ErrorIs(t, runCtx.Err(), context.Canceled)
				require.NoError(t, err)
				require.Equal(t, account.GetOpenAIAccessToken(), token)
				require.Equal(t, 1, db.clearErrorCalls)
			} else {
				require.ErrorIs(t, err, refreshErr)
				require.Empty(t, token)
				require.Zero(t, db.clearErrorCalls)
			}
			require.Same(t, refreshErr, api.openAIRefreshFailure(account))
			require.Zero(t, executor.calls.Load())
		})
	}
}

func TestAccountTestService_OpenAIUnauthorizedRevokesVerifiedAccess(t *testing.T) {
	account, repo, _, provider := newOpenAIRecoveryFixture()
	api := provider.refreshAPI
	api.recordOpenAIRefreshFailure(account, &openai.RefreshTokenError{Permanent: true})
	api.verifyOpenAIAccessAfterTest(context.Background(), account, api.openAIRejectionForAccount(account), "Bearer "+account.GetOpenAIAccessToken(), nil)
	gateway := &OpenAIGatewayService{openAITokenProvider: provider}
	require.False(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6.2-astra", false))
	upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		resp := newJSONResponse(http.StatusUnauthorized, "{}")
		resp.Request = req
		return resp, nil
	}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, openaiGatewayService: gateway}
	ctx, _ := newTestContext()
	require.Error(t, svc.TestAccountConnection(ctx, account.ID, "gpt-6.2-astra", "", AccountTestModeDefault))
	require.True(t, gateway.isOpenAIAccountRequestRuntimeBlocked(account, "gpt-6.2-astra", false))
}
