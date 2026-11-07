//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type codexSourceAuthClient struct {
	OpenAIOAuthClient
	endpoint string
}

func (c *codexSourceAuthClient) RefreshTokenWithClientID(ctx context.Context, _, _, _ string) (*openai.TokenResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(`{"synthetic_refresh":true}`))
	if err != nil {
		return nil, err
	}
	ApplyCodexAuthIdentityForContext(ctx, request.Header)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return &openai.TokenResponse{AccessToken: "repaired-access", RefreshToken: "rotated-refresh", ExpiresIn: 3600}, nil
}

func TestCodexSourceBuildAuthIdentityIsRequestAndAccountBound(t *testing.T) {
	codexSourceTestState(t)
	account, _, _, _ := newOpenAIRecoveryFixture()
	headers := codexSourceTestContext(codexSourceTestUA, "codex_cli_rs", "0.0.0").Request.Header
	base := context.Background()
	bound := withCodexSourceBuildAuthIdentity(base, account, headers)
	ua, _ := CodexAuthIdentityForContext(bound)
	require.Contains(t, ua, "/0.166.0", "private claim alone must match the actual auth account")
	for _, id := range []int64{account.ID, account.ID + 1} {
		ua, origin := CodexAuthIdentityForContext(openai.WithCodexAuthAccount(bound, id))
		if id == account.ID {
			require.Equal(t, codexSourceTestUA, ua)
			require.Equal(t, "codex_cli_rs", origin)
		} else {
			require.Contains(t, ua, "/0.166.0")
		}
	}
	ua, _ = CodexAuthIdentityForContext(openai.WithCodexAuthAccount(base, account.ID))
	require.Contains(t, ua, "/0.166.0", "ordinary refresh cannot inherit a prior request claim")
	unbound := withCodexSourceBuildAuthIdentity(bound, account, http.Header{"User-Agent": {"curl/0.0.0"}, "Originator": {"codex_cli_rs"}})
	ua, _ = CodexAuthIdentityForContext(openai.WithCodexAuthAccount(unbound, account.ID))
	require.Contains(t, ua, "/0.166.0", "invalid replacement headers clear inherited request claims")
}

func TestCodexSourceBuildUnauthorizedRecoveryKeepsIdentityWithoutPersistingIt(t *testing.T) {
	codexSourceTestState(t)
	for _, sourceBuild := range []bool{true, false} {
		t.Run(map[bool]string{true: "source_build", false: "ordinary_old_stable"}[sourceBuild], func(t *testing.T) {
			account, repo, executor, provider := newOpenAIRecoveryFixture()
			account.Extra = map[string]any{codexClientFamilyExtraKey: codexClientFamilyExec}
			repo.account = snapshotOAuthRefreshAccount(account)
			beforeExtra := shallowCopyMap(account.Extra)
			observedAuth := make(chan http.Header, 2)
			var refreshCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				refreshCalls.Add(1)
				observedAuth <- request.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"synthetic":true}`)
			}))
			defer server.Close()
			oauth := NewOpenAIOAuthService(nil, &codexSourceAuthClient{endpoint: server.URL + "/oauth/token"})
			executor.refresh = func(ctx context.Context, latest *Account) (map[string]any, error) {
				info, err := oauth.RefreshAccountToken(ctx, latest)
				if err != nil {
					return nil, err
				}
				updated := shallowCopyMap(latest.Credentials)
				updated["access_token"], updated["refresh_token"] = info.AccessToken, info.RefreshToken
				return updated, nil
			}
			ua, version := codexSourceTestUA, "0.0.0"
			if !sourceBuild {
				ua, version = strings.ReplaceAll(ua, "0.0.0", "0.137.0"), "0.137.0"
			}
			headers := codexSourceTestContext(ua, "codex_cli_rs", version).Request.Header
			calls := 0
			upstream := &openAIRecoveryUpstream{do: func(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				calls++
				require.Equal(t, ua, request.Header.Get("User-Agent"))
				require.Equal(t, version, request.Header.Get("Version"))
				status := http.StatusUnauthorized
				if calls == 3 {
					status = http.StatusOK
					require.Equal(t, "Bearer repaired-access", request.Header.Get("Authorization"))
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"synthetic":true}`))}, nil
			}}
			svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream, openAITokenProvider: provider}
			request, err := http.NewRequest(http.MethodPost, chatgptCodexURL, strings.NewReader(`{"model":"gpt-6-astra","input":"synthetic"}`))
			require.NoError(t, err)
			request.Header = headers
			request.Header.Set("Authorization", "Bearer rejected-access")
			response, err := svc.doOpenAIUpstream(request, "", account)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusOK, response.StatusCode)
			require.Equal(t, 3, calls, "original + guarded reload + one refresh retry")
			require.Equal(t, int32(1), refreshCalls.Load())
			authHeaders := <-observedAuth
			require.Equal(t, int32(1), executor.calls.Load())
			if sourceBuild {
				require.Equal(t, codexSourceTestUA, authHeaders.Get("User-Agent"))
				require.Equal(t, "codex_cli_rs", authHeaders.Get("Originator"))
			} else {
				require.Contains(t, authHeaders.Get("User-Agent"), "codex_exec/0.166.0")
				require.Equal(t, "codex_exec", authHeaders.Get("Originator"))
			}
			require.Empty(t, authHeaders.Get("Version"), "credential plane has no version header")
			require.Equal(t, beforeExtra, account.Extra)
			require.Equal(t, beforeExtra, repo.account.Extra, "source identity never persists in device/account state")
			require.Equal(t, "Bearer rejected-access", request.Header.Get("Authorization"))
		})
	}
}

func TestCodexSourceBuildWSRecoveryCarriesOnlyCurrentHeaderIdentity(t *testing.T) {
	codexSourceTestState(t)
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	executor.refresh = func(ctx context.Context, latest *Account) (map[string]any, error) {
		ua, origin := CodexAuthIdentityForContext(openai.WithCodexAuthAccount(ctx, latest.ID))
		require.Equal(t, codexSourceTestUA, ua)
		require.Equal(t, "codex_cli_rs", origin)
		credentials := shallowCopyMap(latest.Credentials)
		credentials["access_token"] = "repaired-access"
		return credentials, nil
	}
	svc := &OpenAIGatewayService{accountRepo: repo, openAITokenProvider: provider}
	headers := codexSourceTestContext(codexSourceTestUA, "codex_cli_rs", "0.0.0").Request.Header
	headers.Set("Authorization", "Bearer rejected-access")
	updated, _, ok := svc.recoverOpenAIOAuthHeaders(context.Background(), account, headers)
	require.True(t, ok)
	require.Equal(t, "Bearer repaired-access", updated.Get("Authorization"))
	require.Equal(t, codexSourceTestUA, updated.Get("User-Agent"))
	require.Equal(t, "0.0.0", updated.Get("Version"))
	require.Equal(t, int32(1), executor.calls.Load())
	require.Equal(t, "Bearer rejected-access", headers.Get("Authorization"))
}
