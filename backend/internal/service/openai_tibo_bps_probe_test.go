package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func tiboBPSProbeAccount() *Account {
	account := excelAccount()
	proxyID := int64(7)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{ID: proxyID, Protocol: "socks5", Host: "bps-probe-proxy.invalid", Port: 1080}
	// The probe always asks gpt-6-astra, whatever the business model mapping says.
	account.Credentials["model_mapping"] = map[string]any{"gpt-6-astra": "gpt-6-sol"}
	return account
}

func tiboBPSCompleted(model, answer string) string {
	return fmt.Sprintf("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bps\",\"status\":\"completed\",\"model\":%q,\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n", model, answer)
}

func tiboBPSResponse(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func requireTiboBPSProbeRequest(t *testing.T, upstream *httpUpstreamRecorder, account *Account) {
	t.Helper()
	require.Len(t, upstream.requests, 1, "exactly one probe, never a fallback route")
	req := upstream.lastReq
	require.Equal(t, http.MethodPost, req.Method)
	require.Equal(t, "bps.openai.com", req.URL.Host)
	require.Equal(t, "/basispoints/api/responses", req.URL.Path)
	require.Equal(t, "Bearer test-token", req.Header.Get("Authorization"))
	require.Equal(t, "test-account", req.Header.Get("Chatgpt-Account-Id"))
	require.Equal(t, HTTPUpstreamProfileLongStream, HTTPUpstreamProfileFromContext(req.Context()))
	require.True(t, HTTPUpstreamRedirectsDisabled(req.Context()))
	require.Equal(t, account.Proxy.URL(), upstream.lastProxyURL, "probe uses the account's own proxy")
	require.Contains(t, string(upstream.lastBody), openAICookieWSProbePrompt)
	require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "low", gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
}

func TestTiboBPSProbeVerdicts(t *testing.T) {
	cut := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_bps\",\"status\":\"in_progress\"}}\n\n"
	deltaOnly := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"True\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bps\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[]}}\n\n"
	for _, tc := range []struct {
		name string
		wire string
		want openAITiboProbeSample
	}{
		{"True is healthy", tiboBPSCompleted("gpt-6-astra", "True"), openAITiboProbeSample{verdict: openAITiboHealthy, status: http.StatusOK, answerClass: tiboAnswerTrue}},
		{"streamed True is healthy", deltaOnly, openAITiboProbeSample{verdict: openAITiboHealthy, status: http.StatusOK, answerClass: tiboAnswerTrue}},
		{"False is degraded", tiboBPSCompleted("gpt-6-astra", "False"), openAITiboProbeSample{verdict: openAITiboDegraded, status: http.StatusOK, answerClass: tiboAnswerFalse}},
		{"another model is degraded", tiboBPSCompleted("gpt-5.6-luna", "True"), openAITiboProbeSample{verdict: openAITiboDegraded, status: http.StatusOK, answerClass: tiboAnswerNone}},
		{"stream cut before completion is unknown", cut, openAITiboProbeSample{verdict: openAITiboUnknown, status: http.StatusOK}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: tiboBPSResponse(http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, tc.wire)}
			svc := openAIClientToolsTestService(upstream)
			account := tiboBPSProbeAccount()
			require.Equal(t, tc.want, svc.probeOpenAITiboBPS(context.Background(), account))
			requireTiboBPSProbeRequest(t, upstream, account)
			require.True(t, account.IsExcelBPSEnabled())
			require.True(t, account.Schedulable)
		})
	}
}

func TestTiboBPSProbeRateLimitedIsUnknownWithRetryAt(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: tiboBPSResponse(http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}}, `{"error":{"code":"rate_limit_exceeded"}}`)}
	svc := openAIClientToolsTestService(upstream)
	account := tiboBPSProbeAccount()
	sample := svc.probeOpenAITiboBPS(context.Background(), account)
	require.Equal(t, openAITiboUnknown, sample.verdict)
	require.Equal(t, http.StatusTooManyRequests, sample.status)
	require.Empty(t, sample.answerClass)
	require.NotNil(t, sample.retryAt)
	require.WithinDuration(t, time.Now().Add(120*time.Second), *sample.retryAt, 5*time.Second)
	requireTiboBPSProbeRequest(t, upstream, account)
}

func TestTiboBPSProbe403DoesNotDisableBPS(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: tiboBPSResponse(http.StatusForbidden, nil, `{"error":{"code":"permission_denied"}}`)}
	svc := openAIClientToolsTestService(upstream)
	svc.accountRepo = &excelBPSAutoDisableRepo{disable: func(context.Context, *Account) (bool, error) {
		t.Fatal("a probe must never change account state")
		return false, nil
	}}
	account := tiboBPSProbeAccount()
	account.Extra["openai_excel_bps_auto_disable_on_403"] = true
	require.Equal(t, openAITiboProbeSample{verdict: openAITiboUnknown, status: http.StatusForbidden}, svc.probeOpenAITiboBPS(context.Background(), account))
	requireTiboBPSProbeRequest(t, upstream, account)
	require.True(t, account.IsExcelBPSEnabled())
	require.True(t, account.Schedulable)
}

func TestTiboBPSProbeTransportErrorIsUnknown(t *testing.T) {
	upstream := &httpUpstreamRecorder{err: syscall.ECONNREFUSED}
	svc := openAIClientToolsTestService(upstream)
	account := tiboBPSProbeAccount()
	require.Equal(t, openAITiboProbeSample{verdict: openAITiboUnknown}, svc.probeOpenAITiboBPS(context.Background(), account))
	requireTiboBPSProbeRequest(t, upstream, account)
}

func TestTiboBPSProbeIsNilSafe(t *testing.T) {
	unknown := openAITiboProbeSample{verdict: openAITiboUnknown}
	var nilService *OpenAIGatewayService
	require.Equal(t, unknown, nilService.probeOpenAITiboBPS(context.Background(), tiboBPSProbeAccount()))
	require.Equal(t, unknown, (&OpenAIGatewayService{}).probeOpenAITiboBPS(context.Background(), tiboBPSProbeAccount()))
	upstream := &httpUpstreamRecorder{}
	require.Equal(t, unknown, openAIClientToolsTestService(upstream).probeOpenAITiboBPS(context.Background(), nil))
	// Without a ChatGPT account ID the request is never sent.
	account := tiboBPSProbeAccount()
	delete(account.Credentials, "chatgpt_account_id")
	require.Equal(t, unknown, openAIClientToolsTestService(upstream).probeOpenAITiboBPS(context.Background(), account))
	require.Empty(t, upstream.requests)
}
