//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRejectedOpenAIAccessProbeHTTP(t *testing.T) {
	const delta = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"
	const terminal = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	const final = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"OK\"}]}]}}\n\n"
	const tool = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"name\":\"ok\"}]}}\n\n"
	for _, tc := range []struct {
		name        string
		code        int
		body        string
		changeFinal func(*http.Request)
		want        openAIAccessProbeOutcome
	}{
		{name: "deltas and completed", code: 200, body: delta + terminal, want: openAIAccessProbeVerified},
		{name: "text only in terminal output", code: 200, body: final, want: openAIAccessProbeVerified},
		{name: "conflicting final text", code: 200, body: delta + strings.ReplaceAll(final, "OK", "different"), want: openAIAccessProbeRetry},
		{name: "delta without terminal", code: 200, body: delta, want: openAIAccessProbeRetry},
		{name: "empty success", code: 200, body: terminal, want: openAIAccessProbeRetry},
		{name: "whitespace output", code: 200, body: strings.ReplaceAll(delta, "OK", "  ") + terminal, want: openAIAccessProbeRetry},
		{name: "tool output instead of assistant text", code: 200, body: delta + tool, want: openAIAccessProbeRetry},
		{name: "failed terminal", code: 200, body: delta + strings.ReplaceAll(terminal, "completed", "failed"), want: openAIAccessProbeRetry},
		{name: "bounded input", code: 200, body: strings.Repeat("x", openAIAccessProbeBodyLimit) + "\n" + final, want: openAIAccessProbeRetry},
		{name: "same credential rejected", code: 401, want: openAIAccessProbeUnauthorized},
		{name: "rate limit", code: 429, want: openAIAccessProbeRetry},
		{name: "provider unavailable", code: 503, want: openAIAccessProbeRetry},
		{name: "permanent non-auth request failure", code: 400, want: openAIAccessProbeSkipped},
		{name: "unexpected host", code: 200, body: final, changeFinal: func(r *http.Request) { r.URL.Host = "other.invalid" }, want: openAIAccessProbeSkipped},
		{name: "different workspace", code: 200, body: final, changeFinal: func(r *http.Request) { r.Header.Set("chatgpt-account-id", "different") }, want: openAIAccessProbeSkipped},
		{name: "ambiguous workspace", code: 200, body: final, changeFinal: func(r *http.Request) { r.Header["chatgpt-account-id"] = []string{"different"} }, want: openAIAccessProbeSkipped},
		{name: "different bearer rejection", code: 401, changeFinal: func(r *http.Request) { r.Header.Set("Authorization", "Bearer other") }, want: openAIAccessProbeSkipped},
		{name: "ambiguous bearer rejection", code: 401, changeFinal: func(r *http.Request) { r.Header.Add("Authorization", "Bearer other") }, want: openAIAccessProbeSkipped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account, repo, executor, _ := newOpenAIRecoveryFixture()
			proxyID := int64(7)
			account.ProxyID, account.Proxy = &proxyID, &Proxy{Protocol: "http", Host: "127.0.0.1", Port: 19000}
			calls := 0
			svc := &AccountTestService{accountRepo: repo, httpUpstream: &codexModelsHTTPUpstreamStub{do: func(req *http.Request, proxy string, id int64, _ int) (*http.Response, error) {
				calls++
				require.Equal(t, account.ID, id)
				require.Equal(t, "http://127.0.0.1:19000", proxy)
				require.Equal(t, chatgptCodexAPIURL, req.URL.String())
				require.Equal(t, http.MethodPost, req.Method)
				require.Equal(t, "Bearer "+account.GetOpenAIAccessToken(), req.Header.Get("Authorization"))
				require.Equal(t, account.GetChatGPTAccountID(), req.Header.Get("chatgpt-account-id"))
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, openAIAccessProbeModel, gjson.GetBytes(body, "model").String())
				require.True(t, gjson.GetBytes(body, "stream").Bool())
				require.False(t, gjson.GetBytes(body, "store").Bool())
				resp := newJSONResponse(tc.code, tc.body)
				resp.Request = req.Clone(req.Context())
				if tc.changeFinal != nil {
					tc.changeFinal(resp.Request)
				}
				return resp, nil
			}}}
			result := svc.probeRejectedOpenAIAccess(context.Background(), account)
			require.Equal(t, tc.want, result.outcome)
			if tc.want == openAIAccessProbeVerified || tc.want == openAIAccessProbeUnauthorized {
				require.Equal(t, "Bearer "+account.GetOpenAIAccessToken(), result.authorization)
			} else {
				require.Empty(t, result.authorization)
			}
			require.Equal(t, 1, calls)
			require.Zero(t, executor.calls.Load())
		})
	}
}

func TestRejectedOpenAIAccessProbeDoesNotSendWithoutConfiguredProxy(t *testing.T) {
	account, repo, _, _ := newOpenAIRecoveryFixture()
	proxyID := int64(7)
	account.ProxyID = &proxyID
	svc := &AccountTestService{accountRepo: repo, httpUpstream: &codexModelsHTTPUpstreamStub{do: func(*http.Request, string, int64, int) (*http.Response, error) {
		t.Fatal("diagnostic must use the configured proxy")
		return nil, nil
	}}}
	require.Equal(t, openAIAccessProbeRetry, svc.probeRejectedOpenAIAccess(context.Background(), account).outcome)
}

func TestRejectedOpenAIAccessProbeSkipsUnattributedPluginRoute(t *testing.T) {
	account, repo, _, _ := newOpenAIRecoveryFixture()
	plugins := &PluginManager{}
	plugins.route.Store(&pluginRoute{rolloutPercent: 100})
	svc := &AccountTestService{accountRepo: repo, pluginManager: plugins, httpUpstream: &codexModelsHTTPUpstreamStub{do: func(*http.Request, string, int64, int) (*http.Response, error) {
		t.Fatal("a plugin binding must not be bypassed")
		return nil, nil
	}}}
	result := svc.probeRejectedOpenAIAccess(context.Background(), account)
	require.Equal(t, openAIAccessProbeSkipped, result.outcome)
	require.Empty(t, result.authorization)
}
