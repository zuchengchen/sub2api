package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Regression coverage for Codex "remote compaction v2 expected exactly one
// compaction output item, got 0 from 0 output items" on gpt-6-astra (Cookie
// WS) and gpt-6-sol (Excel BPS): native v2 compaction must not be routed to a
// transport without a compaction contract, and a zero-item compaction stream
// must never reach the client as a success.

const nativeCompactionBody = `{"model":"gpt-6-astra","stream":true,"instructions":"compact-test","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"compaction_trigger"}]}`

func excelAccount() *Account {
	return &Account{
		ID: 300, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 10,
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"},
		Extra:       map[string]any{"openai_passthrough": true},
	}
}

func nativeCompactionSSE(withItem bool) string {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cmp\",\"status\":\"in_progress\",\"output\":[]}}\n\n"
	usage := `"usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150}`
	if !withItem {
		return created + "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_cmp\",\"status\":\"completed\",\"output\":[]," + usage + "}}\n\n"
	}
	item := `{"type":"compaction","id":"cmp_1","encrypted_content":"gAAAAcompacted"}`
	return created +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":" + item + "}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + item + "}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_cmp\",\"status\":\"completed\",\"output\":[" + item + "]," + usage + "}}\n\n"
}

func nativeCompactionSSEResponse(withItem bool) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(nativeCompactionSSE(withItem)))}
}

func newNativeCompactionContext(path string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(nativeCompactionBody)))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	MarkOpenAINativeCompactionV2(c)
	return c, rec
}

func TestOpenAINativeCompactionTerminalMissingItem(t *testing.T) {
	gin.SetMode(gin.TestMode)
	native, _ := gin.CreateTestContext(httptest.NewRecorder())
	MarkOpenAINativeCompactionV2(native)
	plain, _ := gin.CreateTestContext(httptest.NewRecorder())

	empty := []byte(`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":5}}}`)
	missing := []byte(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5}}}`)
	withItem := []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"compaction","encrypted_content":"x"}]}}`)
	failed := []byte(`{"type":"response.completed","response":{"status":"failed","output":[],"error":{"code":"x"}}}`)

	require.True(t, openAINativeCompactionTerminalMissingItem(native, "response.completed", empty))
	require.True(t, openAINativeCompactionTerminalMissingItem(native, "response.done", missing))
	require.False(t, openAINativeCompactionTerminalMissingItem(native, "response.completed", withItem))
	require.False(t, openAINativeCompactionTerminalMissingItem(native, "response.completed", failed))
	require.False(t, openAINativeCompactionTerminalMissingItem(native, "response.output_item.done", empty))
	require.False(t, openAINativeCompactionTerminalMissingItem(plain, "response.completed", empty), "ordinary turns keep the existing empty-completed rules")
	require.False(t, openAINativeCompactionTerminalMissingItem(native, "response.completed", []byte(`{"type":`)))
}

// The existing #5009 guard ignores terminals with usage; a native compaction
// turn that billed tokens but produced no item must still fail over.
func TestOpenAINativeCompactionZeroItemsFailsOverOnHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{true, false} {
		t.Run(map[bool]string{true: "passthrough", false: "oauth_transform"}[passthrough], func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: nativeCompactionSSEResponse(false)}
			svc := openAIClientToolsTestService(upstream)
			account := excelAccount()
			account.Extra = map[string]any{"openai_passthrough": passthrough}
			c, rec := newNativeCompactionContext("/v1/responses")

			_, err := svc.Forward(context.Background(), c, account, []byte(nativeCompactionBody))
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.True(t, errors.As(err, &failoverErr), "zero-item compaction must be a failover, got: %v", err)
			require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
			require.Equal(t, openAINativeCompactionMissingItemCode, gjson.GetBytes(failoverErr.ResponseBody, "error.code").String())
			require.NotContains(t, rec.Body.String(), "response.completed", "the empty success stream must not reach Codex")
		})
	}
}

func TestOpenAINativeCompactionWithItemSucceedsOnHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{true, false} {
		t.Run(map[bool]string{true: "passthrough", false: "oauth_transform"}[passthrough], func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: nativeCompactionSSEResponse(true)}
			svc := openAIClientToolsTestService(upstream)
			account := excelAccount()
			account.Extra = map[string]any{"openai_passthrough": passthrough}
			c, rec := newNativeCompactionContext("/v1/responses")

			result, err := svc.Forward(context.Background(), c, account, []byte(nativeCompactionBody))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Contains(t, rec.Body.String(), `"type":"compaction"`)
			require.Contains(t, rec.Body.String(), "response.completed")
		})
	}
}

// Ordinary (non-compaction) turns with usage but no output keep their
// existing success behavior.
func TestOpenAINativeCompactionGuardIgnoresOrdinaryTurns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: nativeCompactionSSEResponse(false)}
	svc := openAIClientToolsTestService(upstream)
	account := excelAccount()
	account.Extra = map[string]any{"openai_passthrough": true}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`)
	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "response.completed")
}

// gpt-6-astra with a ready Cookie WS ticket: native v2 compaction must use the
// HTTP Responses route, like the legacy /compact path already does.
func TestCookieWSNativeCompactionUsesHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, account, _, dialer := newCookieForwardFixture(t, &openAIWSCaptureConn{events: [][]byte{[]byte(tiboRouteWSCompleted)}})
	upstream := mustTestValue[*httpUpstreamRecorder](t, svc.httpUpstream)
	upstream.resp = nativeCompactionSSEResponse(true)
	c, rec := newNativeCompactionContext("/v1/responses")

	result, err := svc.Forward(context.Background(), c, account, []byte(nativeCompactionBody))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Zero(t, dialer.DialCount(), "native compaction must not dial Cookie WS")
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
	require.Contains(t, upstream.lastReq.Header.Get("x-codex-beta-features"), "remote_compaction_v2")
	require.Contains(t, rec.Body.String(), `"type":"compaction"`)
}

// Ordinary Astra turns on the same fixture still use Cookie WS.
func TestCookieWSOrdinaryTurnStillUsesCookieWS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, account, _, dialer := newCookieForwardFixture(t, &openAIWSCaptureConn{events: [][]byte{[]byte(tiboRouteWSCompleted)}})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.True(t, result.OpenAIWSMode)
	require.Equal(t, 1, dialer.DialCount())
}

func TestTiboRouteTiersUseTicketNotCookieWS(t *testing.T) {
	tc := newTiboRouteCase(t, false, true)
	tiers := tc.svc.openAITiboRouteTiers(tc.account, "gpt-6-astra", false, openAITiboDegraded)
	_, hasWS := tiers[openAITiboRouteCookieWS]
	require.False(t, hasWS, "Cookie WS is not a Tibo plan hop")
	_, hasTicket := tiers[openAITiboRouteTicket]
	require.True(t, hasTicket, "ordinary plan keeps the ticketed /responses hop")

	c, _ := newNativeCompactionContext("/v1/responses")
	run := tc.svc.newOpenAITiboRun(context.Background(), c, tc.account, []byte(nativeCompactionBody), "scope-native-compaction")
	_, hasWS = run.tiers[openAITiboRouteCookieWS]
	require.False(t, hasWS)
	require.NotEqual(t, openAITiboRouteCookieWS, run.first())
}

func TestOpenAINativeCompactionStreamInterval(t *testing.T) {
	gin.SetMode(gin.TestMode)
	native, _ := gin.CreateTestContext(httptest.NewRecorder())
	MarkOpenAINativeCompactionV2(native)
	plain, _ := gin.CreateTestContext(httptest.NewRecorder())

	require.Equal(t, 600*time.Second, openAINativeCompactionStreamInterval(native, 180*time.Second), "compaction must outlast Codex's 300s SSE idle timeout")
	require.Equal(t, 900*time.Second, openAINativeCompactionStreamInterval(native, 900*time.Second), "a longer configured budget is kept")
	require.Equal(t, time.Duration(0), openAINativeCompactionStreamInterval(native, 0), "a disabled timeout stays disabled")
	require.Equal(t, 180*time.Second, openAINativeCompactionStreamInterval(plain, 180*time.Second), "ordinary turns keep the configured timeout")
	require.Equal(t, 180*time.Second, openAINativeCompactionStreamInterval(nil, 180*time.Second))
	require.Greater(t, openAINativeCompactionMinStreamInterval, 300*time.Second)
}

// silentCompactionUpstream emits the response preamble, stays silent for the
// given duration (summarising a large context emits nothing), then sends the
// compaction item and completion.
func silentCompactionUpstream(silence time.Duration) *http.Response {
	pr, pw := io.Pipe()
	wire := nativeCompactionSSE(true)
	split := strings.Index(wire, "event: response.output_item.added")
	go func() {
		_, _ = io.WriteString(pw, wire[:split])
		time.Sleep(silence)
		_, _ = io.WriteString(pw, wire[split:])
		_ = pw.Close()
	}()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}
}

// Production: a 160K-token gpt-6-astra compaction stayed silent past the 180s
// stream_data_interval_timeout; the gateway wrote a bare stream_timeout error
// frame (ignored by Codex) and committed the response, so the turn failed.
func TestOpenAINativeCompactionSurvivesSilenceBeyondStreamInterval(t *testing.T) {
	gin.SetMode(gin.TestMode)
	prev := openAINativeCompactionMinStreamInterval
	openAINativeCompactionMinStreamInterval = 5 * time.Second
	t.Cleanup(func() { openAINativeCompactionMinStreamInterval = prev })

	upstream := &httpUpstreamRecorder{resp: silentCompactionUpstream(2500 * time.Millisecond)}
	svc := openAIClientToolsTestService(upstream)
	svc.cfg.Gateway.StreamDataIntervalTimeout = 1
	account := excelAccount()
	account.Extra = map[string]any{"openai_passthrough": false}
	c, rec := newNativeCompactionContext("/v1/responses")

	result, err := svc.Forward(context.Background(), c, account, []byte(nativeCompactionBody))
	require.NoError(t, err)
	require.NotNil(t, result)
	out := rec.Body.String()
	require.NotContains(t, out, "stream_timeout")
	require.Contains(t, out, "event: response.output_item.done")
	require.Contains(t, out, `"type":"compaction"`)
	require.Contains(t, out, "event: response.completed")
}

func TestOrdinaryTurnStillTimesOutAfterStreamInterval(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{resp: silentCompactionUpstream(2500 * time.Millisecond)}
	svc := openAIClientToolsTestService(upstream)
	svc.cfg.Gateway.StreamDataIntervalTimeout = 1
	account := excelAccount()
	account.Extra = map[string]any{"openai_passthrough": false}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream data interval timeout")
	require.Contains(t, rec.Body.String(), "stream_timeout")
}
