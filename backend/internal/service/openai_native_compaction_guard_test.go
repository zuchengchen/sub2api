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

// gpt-6-sol on Excel BPS: a BPS compaction stream without an item is held
// back and the same account serves the turn on Codex HTTP.
func TestExcelBPSNativeCompactionWithoutItemFallsBackToHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{nativeCompactionSSEResponse(false), nativeCompactionSSEResponse(true)}}
	svc := openAIClientToolsTestService(upstream)
	c, rec := newNativeCompactionContext("/v1/responses")
	body := []byte(strings.Replace(nativeCompactionBody, "gpt-6-astra", "gpt-6-sol", 1))

	result, err := svc.Forward(context.Background(), c, excelAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, "bps.openai.com", upstream.requests[0].URL.Host)
	require.Equal(t, "compaction_trigger", gjson.GetBytes(upstream.bodies[0], "input.@reverse.0.type").String())
	require.Equal(t, "chatgpt.com", upstream.requests[1].URL.Host)
	require.Equal(t, excelBPSHTTPFallbackReason, c.GetString(openAIBPSBypassReasonKey))
	out := rec.Body.String()
	require.Equal(t, 1, strings.Count(out, "event: response.completed"), "only the HTTP stream may reach the client")
	require.Contains(t, out, `"type":"compaction"`)
	require.NotEqual(t, "/basispoints/api/responses", result.UpstreamEndpoint)
}

func TestExcelBPSNativeCompactionWithItemStaysOnBPS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{nativeCompactionSSEResponse(true)}}
	svc := openAIClientToolsTestService(upstream)
	c, rec := newNativeCompactionContext("/v1/responses")
	body := []byte(strings.Replace(nativeCompactionBody, "gpt-6-astra", "gpt-6-sol", 1))

	result, err := svc.Forward(context.Background(), c, excelAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "/basispoints/api/responses", result.UpstreamEndpoint)
	out := rec.Body.String()
	require.Contains(t, out, `"type":"compaction"`)
	require.Less(t, strings.Index(out, "response.created"), strings.Index(out, "response.completed"), "held lines are released in order")
}

func TestExcelBPSNativeCompactionHoldReleasesOversizedStream(t *testing.T) {
	hold := &excelBPSNativeCompactionHold{}
	require.True(t, hold.add("data: small"))
	require.False(t, hold.add(strings.Repeat("x", excelBPSNativeCompactionHoldLimit)))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	require.NoError(t, hold.release(c))
	require.True(t, strings.HasPrefix(rec.Body.String(), "data: small\n"))
	require.Nil(t, hold.lines)
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

func TestTiboRouteTiersExcludeCookieWSForNativeCompaction(t *testing.T) {
	tc := newTiboRouteCase(t, false, true)
	cfg := tc.svc.openAITiboRouteConfig()
	tiers := tc.svc.openAITiboRouteTiers(tc.account, "gpt-6-astra", []byte(nativeCompactionBody), false, openAITiboDegraded, openAITiboUnknown, cfg)
	_, hasWS := tiers[openAITiboRouteCookieWS]
	require.True(t, hasWS, "ordinary plan keeps the ready Cookie WS route")

	c, _ := newNativeCompactionContext("/v1/responses")
	run := tc.svc.newOpenAITiboRun(context.Background(), c, tc.account, []byte(nativeCompactionBody), "scope-native-compaction")
	_, hasWS = run.tiers[openAITiboRouteCookieWS]
	require.False(t, hasWS, "native compaction plan must not include Cookie WS")
	require.Equal(t, openAITiboRouteHTTP, run.first())
}
