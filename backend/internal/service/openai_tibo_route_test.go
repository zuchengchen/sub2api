package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const tiboRouteWSCompleted = `{"type":"response.completed","response":{"id":"resp_ws","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ws ok"}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`

func tiboRouteBPSResponse() *http.Response {
	wire := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bps\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"bps ok\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
}

func tiboRouteStatusResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"denied"}}`))}
}

type tiboRouteCase struct {
	svc      *OpenAIGatewayService
	account  *Account
	upstream *httpUpstreamRecorder
	dialer   *cookieForwardDialer
}

// newTiboRouteCase enables Tibo routing on the Cookie WS forward fixture.
// wsReady=false removes the Cookie ticket so WS is unavailable.
func newTiboRouteCase(t *testing.T, bps, wsReady bool, responses ...*http.Response) *tiboRouteCase {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc, account, ticket, dialer := newCookieForwardFixture(t, &openAIWSCaptureConn{events: [][]byte{[]byte(tiboRouteWSCompleted)}})
	svc.tiboRouteDisabled = false
	if bps {
		account.Extra["openai_excel_bps"] = true
	}
	if !wsReady {
		svc.openaiCookieWSTickets.Delete(openAICodexTicketKey(account.ID, ticket.Model))
	}
	upstream := mustTestValue[*httpUpstreamRecorder](t, svc.httpUpstream)
	upstream.responses = responses
	return &tiboRouteCase{svc: svc, account: account, upstream: upstream, dialer: dialer}
}

func (tc *tiboRouteCase) forward(t *testing.T) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := tc.svc.Forward(context.Background(), c, tc.account, []byte(`{"model":"gpt-6-astra","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	return result, rec, c
}

func (tc *tiboRouteCase) hosts() []string {
	hosts := make([]string, 0, len(tc.upstream.requests))
	for _, req := range tc.upstream.requests {
		hosts = append(hosts, req.URL.Host)
	}
	return hosts
}

func requireTiboProbe(t *testing.T, body []byte) {
	t.Helper()
	require.Contains(t, string(body), openAICookieWSProbePrompt)
	require.Contains(t, string(body), `"model":"gpt-6-astra"`)
	require.Contains(t, string(body), `"effort":"low"`)
}

func TestTiboRouteHTTPTrueStaysOnHTTP(t *testing.T) {
	for _, bps := range []bool{false, true} {
		tc := newTiboRouteCase(t, bps, true, cookieWSHTTPResponse("true."), cookieWSHTTPResponse("http ok"))
		result, rec, c := tc.forward(t)
		require.False(t, result.OpenAIWSMode)
		require.Zero(t, tc.dialer.DialCount(), "healthy HTTP must not use Cookie WS")
		require.Equal(t, []string{"chatgpt.com", "chatgpt.com"}, tc.hosts(), "probe, then business HTTP; BPS skipped")
		requireTiboProbe(t, tc.upstream.bodies[0])
		require.Contains(t, string(tc.upstream.bodies[1]), "hello")
		require.Equal(t, openAITiboHTTPOKReason, c.GetString("openai_ws_transport_reason"))
		require.Contains(t, rec.Body.String(), "http ok")
		if bps {
			require.Equal(t, openAITiboHTTPOKReason, c.GetString(openAIBPSBypassReasonKey))
		}
		require.NotNil(t, result.RouteDegraded)
		require.False(t, *result.RouteDegraded)
		requireNoOpenAIRoutingHeaders(t, rec.Header())
	}
}

func TestTiboRouteDegradedPrefersBPS(t *testing.T) {
	tc := newTiboRouteCase(t, true, true, cookieWSHTTPResponse("False"), tiboRouteBPSResponse())
	result, rec, _ := tc.forward(t)
	require.Equal(t, []string{"chatgpt.com", "bps.openai.com"}, tc.hosts())
	require.Equal(t, "/basispoints/api/responses", result.UpstreamEndpoint)
	require.Zero(t, tc.dialer.DialCount())
	require.Contains(t, rec.Body.String(), "bps ok")
}

func TestTiboRouteBPSUnusableFallsBackToCookieWS(t *testing.T) {
	tc := newTiboRouteCase(t, true, true, cookieWSHTTPResponse("False"), tiboRouteStatusResponse(http.StatusForbidden))
	result, rec, c := tc.forward(t)
	require.True(t, result.OpenAIWSMode)
	require.Equal(t, []string{"chatgpt.com", "bps.openai.com"}, tc.hosts())
	require.Equal(t, 1, tc.dialer.DialCount())
	require.Equal(t, "bps_error", c.GetString(openAIBPSBypassReasonKey))
	requireNoOpenAIRoutingHeaders(t, rec.Header())
	require.Contains(t, rec.Body.String(), "ws ok")
}

func TestTiboRouteDegradedWithoutBPSUsesCookieWS(t *testing.T) {
	tc := newTiboRouteCase(t, false, true, cookieWSHTTPResponse("False"))
	result, rec, _ := tc.forward(t)
	require.True(t, result.OpenAIWSMode)
	require.Equal(t, []string{"chatgpt.com"}, tc.hosts())
	require.Contains(t, rec.Body.String(), "ws ok")
}

func TestTiboRouteEverythingUnusableStillSendsHTTP(t *testing.T) {
	tc := newTiboRouteCase(t, true, false, cookieWSHTTPResponse("False"), tiboRouteStatusResponse(http.StatusForbidden), cookieWSHTTPResponse("http ok"))
	result, rec, c := tc.forward(t)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, []string{"chatgpt.com", "bps.openai.com", "chatgpt.com"}, tc.hosts())
	require.Zero(t, tc.dialer.DialCount())
	require.Equal(t, excelBPSHTTPFallbackReason, c.GetString("openai_ws_transport_reason"))
	require.Contains(t, rec.Body.String(), "http ok")
	require.NotNil(t, result.RouteDegraded)
	require.True(t, *result.RouteDegraded, "only degraded routes remain; recorded in usage, not headers")
	requireNoOpenAIRoutingHeaders(t, rec.Header())
}

// A cold account whose first probe is unknown has no confirmed verdict, so it
// still takes the degraded-first chain (Cookie WS here), and unknown does not
// become a verdict.
func TestTiboRouteProbeFailureIsNotHealthy(t *testing.T) {
	tc := newTiboRouteCase(t, false, true, tiboRouteStatusResponse(http.StatusTooManyRequests))
	result, _, _ := tc.forward(t)
	require.True(t, result.OpenAIWSMode, "an unknown verdict takes the degraded chain")
	state := tc.svc.loadOpenAITiboAccountState(tc.account.ID)
	require.NotNil(t, state)
	require.Empty(t, state.http.verdict, "unknown samples never become a verdict")
	require.Equal(t, 1, state.http.unknownStreak)
	require.Equal(t, openAITiboUnknown, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
	require.NotNil(t, result.RouteDegraded)
	require.False(t, *result.RouteDegraded, "Cookie WS served, not the degraded HTTP fallback")
}

// tiboSafeUpstream serves probe and business requests from separate queues
// and is safe for a background probe racing a business request.
type tiboSafeUpstream struct {
	mu         sync.Mutex
	probeGate  chan struct{}
	probes     int
	businesses int
}

func (u *tiboSafeUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	if strings.Contains(string(body), openAICookieWSProbePrompt) {
		if u.probeGate != nil {
			<-u.probeGate
		}
		u.mu.Lock()
		u.probes++
		u.mu.Unlock()
		return cookieWSHTTPResponse("True"), nil
	}
	u.mu.Lock()
	u.businesses++
	u.mu.Unlock()
	return cookieWSHTTPResponse("http ok"), nil
}

func (u *tiboSafeUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

// One probe serves many requests. A due verdict is served immediately while a
// background probe refreshes it; only a never-sampled account waits.
func TestTiboRouteVerdictIsCachedAndRefreshedWhenStale(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("True"), cookieWSHTTPResponse("http ok"), cookieWSHTTPResponse("http ok"))
	tc.forward(t)
	tc.forward(t)
	require.Len(t, tc.upstream.requests, 3, "one probe for two requests")
	requireTiboProbe(t, tc.upstream.bodies[0])
	state := tc.svc.loadOpenAITiboAccountState(tc.account.ID)
	require.Equal(t, openAITiboHealthy, state.http.verdict)
	require.WithinDuration(t, time.Now().Add(10*time.Minute), state.http.nextProbeAt, time.Minute, "healthy cadence")

	safe := &tiboSafeUpstream{probeGate: make(chan struct{})}
	tc.svc.httpUpstream = safe
	state.mu.Lock()
	state.http.nextProbeAt = time.Now().Add(-time.Second)
	checkedBefore := state.http.checkedAt
	state.mu.Unlock()
	// The probe is held until the request has finished: the request must not wait for it.
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := tc.svc.Forward(context.Background(), c, tc.account, []byte(`{"model":"gpt-6-astra","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode, "served on the confirmed verdict")
	require.NotNil(t, result.RouteDegraded)
	require.False(t, *result.RouteDegraded, "healthy HTTP is not the degraded fallback")
	close(safe.probeGate)
	tc.svc.openaiTiboProbeWG.Wait()
	safe.mu.Lock()
	require.Equal(t, 1, safe.probes)
	require.Equal(t, 1, safe.businesses)
	safe.mu.Unlock()
	state.mu.Lock()
	defer state.mu.Unlock()
	require.True(t, state.http.checkedAt.After(checkedBefore), "the agreeing probe refreshed the verdict")
}

func TestTiboRouteModelSwapIsDegraded(t *testing.T) {
	tc := newTiboRouteCase(t, false, true)
	body := "data: " + string(cookieWSCompletion("gpt-5.6-luna", "True")) + "\n\n"
	tc.upstream.responses = []*http.Response{{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}}
	sample := tc.svc.probeOpenAITiboHTTP(context.Background(), tc.account)
	require.Equal(t, http.StatusOK, sample.status)
	require.Equal(t, openAITiboDegraded, sample.verdict)
	require.Contains(t, tc.upstream.lastProxyURL, "bound-proxy.invalid", "probe uses the account's own proxy")
}

func TestTiboRouteProbeRetryAfterIsKept(t *testing.T) {
	tc := newTiboRouteCase(t, false, true)
	resp := tiboRouteStatusResponse(http.StatusTooManyRequests)
	resp.Header.Set("Retry-After", "600")
	tc.upstream.responses = []*http.Response{resp}
	sample := tc.svc.probeOpenAITiboHTTP(context.Background(), tc.account)
	require.Equal(t, openAITiboUnknown, sample.verdict)
	require.Equal(t, http.StatusTooManyRequests, sample.status)
	require.NotNil(t, sample.retryAt)
	require.WithinDuration(t, time.Now().Add(10*time.Minute), *sample.retryAt, 5*time.Second)
}
