//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the actual internal 401 reload, not just the guard in isolation.
func TestNativeReasoningReplayOAuthReloadPrincipalBoundary(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "same_principal_refresh"
		if changed {
			name = "same_account_new_principal"
		}
		t.Run(name, func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			original := nativeChatJWT("owner-a", 100)
			latest := nativeChatJWT("owner-a", 200)
			if changed {
				latest = nativeChatJWT("owner-b", 200)
			}
			account.Credentials["access_token"] = original
			repo.account = snapshotOAuthRefreshAccount(account)
			repo.account.Credentials["access_token"] = latest
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"expired"}}`))},
				{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))},
			}}
			svc := &OpenAIGatewayService{openAITokenProvider: provider, httpUpstream: upstream}
			req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, strings.NewReader(`{"model":"gpt-6-astra","input":[]}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+original)
			req = req.WithContext(context.WithValue(req.Context(), nativeReasoningAuthGuardKey{}, &nativeReasoningAuthGuard{principal: nativeReasoningTokenPrincipal(original)}))
			response, err := svc.doOpenAIUpstream(req, "", account)
			require.EqualValues(t, 0, executor.calls.Load(), "guarded durable reload must not rotate the refresh token")
			if changed {
				require.ErrorIs(t, err, errNativeReasoningCredentialChanged)
				require.Nil(t, response)
				require.Len(t, upstream.requests, 1, "new principal must not receive the previous owner's restored state")
			} else {
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode)
				require.Len(t, upstream.requests, 2)
				require.Equal(t, "Bearer "+latest, upstream.lastReq.Header.Get("Authorization"))
				_ = response.Body.Close()
			}
		})
	}
}
