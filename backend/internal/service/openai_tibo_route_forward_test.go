package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const tiboRouteAstraBody = `{"model":"gpt-6-astra","input":"hello","stream":false}`

// seed puts a confirmed, fresh verdict on route that is not due for a probe,
// so a Forward call never starts one.
func (tc *tiboRouteCase) seed(route openAITiboRoute, verdict openAITiboVerdict) {
	now := time.Now()
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	defer state.mu.Unlock()
	r := state.route(route)
	r.verdict, r.checkedAt, r.flippedAt = verdict, now, now.Add(-time.Hour)
	r.lastSampleAt, r.nextProbeAt = now, now.Add(time.Hour)
}

func (tc *tiboRouteCase) forwardWith(t *testing.T, body string, headers map[string]string) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for key, value := range headers {
		c.Request.Header.Set(key, value)
	}
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := tc.svc.Forward(context.Background(), c, tc.account, []byte(body))
	return result, rec, c, err
}

func tiboRouteModelResponse(model string) *http.Response {
	body := "data: " + string(cookieWSCompletion(model, "http ok")) + "\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestTiboRouteSessionPinsHTTPAcrossTurns(t *testing.T) {
	tc := newTiboRouteCase(t, false, true, cookieWSHTTPResponse("http ok"), cookieWSHTTPResponse("http ok"), cookieWSHTTPResponse("http ok"))
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	session := map[string]string{"session_id": "pin-session-1"}
	result, rec, _, err := tc.forwardWith(t, tiboRouteAstraBody, session)
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	require.Contains(t, rec.Body.String(), "http ok")
	require.Equal(t, openAITiboRouteHTTP, tc.svc.loadOpenAITiboPin(tc.account.ID, openAITiboPinScopeForTest(t, session), time.Now()))

	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	result, rec, _, err = tc.forwardWith(t, tiboRouteAstraBody, session)
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), "http ok")
	result, rec, _, err = tc.forwardWith(t, tiboRouteAstraBody, map[string]string{"session_id": "pin-session-2"})
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, []string{"chatgpt.com", "chatgpt.com", "chatgpt.com"}, tc.hosts())
	require.False(t, result.OpenAIWSMode)
	requireNoOpenAIRoutingHeaders(t, rec.Header())
	require.Zero(t, tc.dialer.DialCount())
}

func TestTiboRoutePinStaysOnHTTPWhenDegraded(t *testing.T) {
	tc := newTiboRouteCase(t, false, true, cookieWSHTTPResponse("http ok"), cookieWSHTTPResponse("http ok"))
	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	session := map[string]string{"session_id": "pin-session-hard"}
	result, rec, _, err := tc.forwardWith(t, tiboRouteAstraBody, session)
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	require.Contains(t, rec.Body.String(), "http ok")
	require.Equal(t, openAITiboRouteHTTP, tc.svc.loadOpenAITiboPin(tc.account.ID, openAITiboPinScopeForTest(t, session), time.Now()))

	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	result, rec, _, err = tc.forwardWith(t, tiboRouteAstraBody, session)
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), "http ok")
	requireNoOpenAIRoutingHeaders(t, rec.Header())
	require.Equal(t, openAITiboRouteHTTP, tc.svc.loadOpenAITiboPin(tc.account.ID, openAITiboPinScopeForTest(t, session), time.Now()))
}

func openAITiboPinScopeForTest(t *testing.T, headers map[string]string) string {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for key, value := range headers {
		c.Request.Header.Set(key, value)
	}
	scope, _ := resolveOpenAIWSExecutionScope(c, []byte(tiboRouteAstraBody), getAPIKeyIDFromContext(c))
	require.NotEmpty(t, scope)
	return scope
}

func TestTiboRouteStaleRouteRecordClearedOnNextAttempt(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("http ok"))
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	// A previous failed attempt on another account left a routing record
	// (and, from an older build, legacy routing headers).
	setOpenAIBPSBypassReason(c, "bps_error")
	c.Header("X-Codex2API-Route-Quality", "degraded")
	c.Header("X-Codex2API-Upstream", "codex")
	c.Header("X-Codex2API-Basispoints-Bypass", "bps_error")
	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	result, err := tc.svc.Forward(context.Background(), c, tc.account, []byte(tiboRouteAstraBody))
	require.NoError(t, err)
	require.False(t, *result.RouteDegraded)
	require.Empty(t, c.GetString(openAIBPSBypassReasonKey))
	requireNoOpenAIRoutingHeaders(t, rec.Header())
}

func TestTiboRouteAstraModelMismatchIsHardEvidence(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, tiboRouteModelResponse("gpt-5.6-luna"), cookieWSHTTPResponse("True"))
	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	result, _, _, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-luna", result.UpstreamResponseModel)
	if tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()) == openAITiboDegraded {
		tc.svc.openaiTiboProbeWG.Wait()
		require.GreaterOrEqual(t, len(tc.upstream.requests), 1)
	}
}

func TestTiboRouteNonAstraMismatchOnlyLogs(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, tiboRouteModelResponse("gpt-6-sol"))
	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	_, _, _, err := tc.forwardWith(t, `{"model":"gpt-5.6-sol","input":"hello","stream":false}`, nil)
	require.NoError(t, err)
	require.Equal(t, openAITiboHealthy, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
}

func TestTiboRouteProbePayloadBusinessRequestIsNotEvidence(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, tiboRouteModelResponse("gpt-5.6-luna"))
	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	body := `{"model":"gpt-6-astra","stream":false,"instructions":"","input":[{"role":"user","content":[{"type":"input_text","text":"` + openAITiboProbePrompt + `"}]}]}`
	_, _, _, err := tc.forwardWith(t, body, nil)
	require.NoError(t, err)
	require.Equal(t, openAITiboHealthy, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
}

func TestTiboRouteHTTPModelMismatchStaysDegraded(t *testing.T) {
	body := "data: " + string(cookieWSCompletion("gpt-5.6-luna", "http ok")) + "\n\n"
	tc := newTiboRouteCase(t, false, true, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))})
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	result, rec, _, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	require.Contains(t, rec.Body.String(), "http ok")
	require.Equal(t, openAITiboDegraded, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
}

func TestTiboStatsDegradedRatioAlert(t *testing.T) {
	cfg := tiboTestSettings()
	cfg.alertRatio, cfg.alertMinRequests = 0.5, 4
	var st openAITiboStats
	for i := 0; i < 3; i++ {
		st.noteServed(true, cfg)
	}
	require.True(t, st.lastAlertAt.IsZero(), "below the minimum sample")
	st.noteServed(true, cfg)
	require.False(t, st.lastAlertAt.IsZero())
	first := st.lastAlertAt
	st.noteServed(true, cfg)
	require.Equal(t, first, st.lastAlertAt, "rate limited")
	require.Equal(t, 5, st.degraded)
}

func TestTiboRouteStatusesInRuntimeView(t *testing.T) {
	tc := newTiboRouteCase(t, false, true)
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	status := OpenAICodexTicketStatus{TiboRoutes: tc.svc.openAITiboRouteStatuses(tc.account, time.Now())}
	require.Len(t, status.TiboRoutes, 1)
	require.Equal(t, "http", status.TiboRoutes[0].Route)
	require.Equal(t, "degraded", status.TiboRoutes[0].Verdict)
	require.Empty(t, tc.upstream.requests, "the admin view never probes")
}
