package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
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

func TestTiboRouteSessionPinsRouteAcrossTurns(t *testing.T) {
	tc := newTiboRouteCase(t, true, true, tiboRouteBPSResponse(), tiboRouteBPSResponse(), cookieWSHTTPResponse("http ok"))
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	session := map[string]string{"session_id": "pin-session-1"}
	result, _, _, err := tc.forwardWith(t, tiboRouteAstraBody, session)
	require.NoError(t, err)
	require.Equal(t, "/basispoints/api/responses", result.UpstreamEndpoint, "degraded HTTP: BPS first")

	// HTTP recovers: the pinned session keeps BPS, a new session takes HTTP.
	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	result, _, _, err = tc.forwardWith(t, tiboRouteAstraBody, session)
	require.NoError(t, err)
	require.Equal(t, "/basispoints/api/responses", result.UpstreamEndpoint, "pinned route stays while healthy")
	result, rec, _, err := tc.forwardWith(t, tiboRouteAstraBody, map[string]string{"session_id": "pin-session-2"})
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, []string{"bps.openai.com", "bps.openai.com", "chatgpt.com"}, tc.hosts())
	require.Equal(t, openAITiboHTTPOKReason, rec.Header().Get("X-Codex2API-Basispoints-Bypass"))
	require.Zero(t, tc.dialer.DialCount())
}

func TestTiboRoutePinSwitchesOnHardFailureAndConfirmedDegrade(t *testing.T) {
	tc := newTiboRouteCase(t, true, false, tiboRouteBPSResponse(), tiboRouteStatusResponse(http.StatusForbidden), cookieWSHTTPResponse("http ok"), tiboRouteBPSResponse())
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	session := map[string]string{"session_id": "pin-session-hard"}
	_, _, _, err := tc.forwardWith(t, tiboRouteAstraBody, session)
	require.NoError(t, err)
	require.Equal(t, openAITiboRouteBPS, tc.svc.loadOpenAITiboPin(tc.account.ID, openAITiboPinScopeForTest(t, session), time.Now()))

	// Hard failure on the pinned route: the next route serves and takes the pin.
	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	result, _, _, err := tc.forwardWith(t, tiboRouteAstraBody, session)
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, openAITiboRouteHTTP, tc.svc.loadOpenAITiboPin(tc.account.ID, openAITiboPinScopeForTest(t, session), time.Now()))

	// The pinned HTTP route is confirmed degraded: the session moves to BPS.
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	result, rec, _, err := tc.forwardWith(t, tiboRouteAstraBody, session)
	require.NoError(t, err)
	require.Equal(t, "/basispoints/api/responses", result.UpstreamEndpoint)
	require.Empty(t, rec.Header().Get(openAITiboRouteQualityHeader))
	require.Equal(t, []string{"bps.openai.com", "bps.openai.com", "chatgpt.com", "bps.openai.com"}, tc.hosts())
	require.Equal(t, openAITiboRouteBPS, tc.svc.loadOpenAITiboPin(tc.account.ID, openAITiboPinScopeForTest(t, session), time.Now()))
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

func TestTiboRouteDegradedHeaderClearedOnNextAttempt(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("http ok"))
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	// A previous failed attempt on another account left routing headers.
	c.Header(openAITiboRouteQualityHeader, "degraded")
	c.Header("X-Codex2API-Basispoints-Bypass", "bps_error")
	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	result, err := tc.svc.Forward(context.Background(), c, tc.account, []byte(tiboRouteAstraBody))
	require.NoError(t, err)
	require.False(t, *result.RouteDegraded)
	require.Empty(t, rec.Header().Get(openAITiboRouteQualityHeader))
	require.Empty(t, rec.Header().Get("X-Codex2API-Basispoints-Bypass"))
}

func TestTiboRouteAstraModelMismatchIsHardEvidence(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, tiboRouteModelResponse("gpt-5.6-luna"), cookieWSHTTPResponse("True"))
	tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
	result, _, _, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-luna", result.UpstreamResponseModel)
	require.Equal(t, openAITiboDegraded, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()), "degraded without a vote")
	tc.svc.openaiTiboProbeWG.Wait()
	require.Len(t, tc.upstream.requests, 2, "a confirmation probe was queued")
	requireTiboProbe(t, tc.upstream.bodies[1])
	state := tc.svc.loadOpenAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	defer state.mu.Unlock()
	require.Equal(t, openAITiboDegraded, state.http.verdict, "one True does not undo it")
	require.Equal(t, []openAITiboVerdict{openAITiboHealthy}, state.http.pending)
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
	body := `{"model":"gpt-6-astra","stream":false,"instructions":"","input":[{"role":"user","content":[{"type":"input_text","text":"` + openAICookieWSProbePrompt + `"}]}]}`
	_, _, _, err := tc.forwardWith(t, body, nil)
	require.NoError(t, err)
	require.Equal(t, openAITiboHealthy, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
}

func TestTiboRouteCookieWSModelMismatchRetiresSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &openAIWSCaptureConn{events: [][]byte{cookieWSCompletion("gpt-5.6-luna", "ws ok")}}
	svc, account, _, dialer := newCookieForwardFixture(t, conn)
	svc.tiboRouteDisabled = false
	tc := &tiboRouteCase{svc: svc, account: account, upstream: mustTestValue[*httpUpstreamRecorder](t, svc.httpUpstream), dialer: dialer}
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	result, rec, _, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
	require.NoError(t, err)
	require.True(t, result.OpenAIWSMode)
	require.Contains(t, rec.Body.String(), "ws ok", "the turn itself is still delivered")
	conn.mu.Lock()
	closed := conn.closed
	conn.mu.Unlock()
	require.True(t, closed, "served by another model: the socket is retired")
	require.Equal(t, [openAICookieWSSlotCount]int{}, svc.getOpenAIWSConnPool().CookieVerifiedCounts(account.ID))
}

func TestTiboRouteLateBPSAfterCookieWSUnavailable(t *testing.T) {
	tc := newTiboRouteCase(t, true, true, tiboRouteBPSResponse())
	tc.svc.cfg.Gateway.OpenAITiboRoute.BPSProbeMode = config.OpenAITiboBPSProbeEnforce
	tc.svc.cfg.Gateway.OpenAIWS.CookieWSHTTPFallbackThresholdBytes = 1 // Cookie WS refuses before sending.
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	state := tc.svc.openAITiboAccountState(tc.account.ID)
	state.mu.Lock()
	state.bps.lastSampleAt, state.bps.nextProbeAt = time.Now(), time.Now().Add(time.Hour) // unknown, not due
	state.mu.Unlock()
	result, rec, _, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
	require.NoError(t, err)
	require.Equal(t, "/basispoints/api/responses", result.UpstreamEndpoint, "cookie_ws -> bps -> http")
	require.Equal(t, []string{"bps.openai.com"}, tc.hosts())
	require.Zero(t, tc.dialer.DialCount())
	require.Empty(t, rec.Header().Get("X-Codex2API-Basispoints-Bypass"), "BPS served; no bypass")
	require.False(t, *result.RouteDegraded)
	require.Contains(t, rec.Body.String(), "bps ok")
}

// With BPS first in the plan, only a definitely-not-sent failure continues on
// this account's next route; a possibly executed one switches accounts.
func TestTiboRouteBPSFailureClassesInPlan(t *testing.T) {
	for _, tc := range []struct {
		name     string
		step     excelBPSWireStep
		failover bool
	}{
		{"refused before send", excelBPSWireStep{err: syscall.ECONNREFUSED}, false},
		{"reset after send", excelBPSWireStep{headersWritten: true, wrote: true, err: syscall.ECONNRESET}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := newTiboRouteCase(t, true, true)
			rc.seed(openAITiboRouteHTTP, openAITiboDegraded)
			upstream := &excelBPSTraceUpstream{steps: []excelBPSWireStep{tc.step}}
			rc.svc.httpUpstream = upstream
			result, rec, _, err := rc.forwardWith(t, tiboRouteAstraBody, nil)
			require.Equal(t, []string{"bps.openai.com"}, upstream.hosts(), "never a second send on this account's HTTP")
			if tc.failover {
				var failoverErr *UpstreamFailoverError
				require.ErrorAs(t, err, &failoverErr)
				require.False(t, failoverErr.RetryableOnSameAccount)
				require.Zero(t, rc.dialer.DialCount(), "Cookie WS is not tried after a possibly executed BPS request")
				require.Empty(t, rec.Body.String())
				return
			}
			require.NoError(t, err)
			require.True(t, result.OpenAIWSMode, "next route on the same account: Cookie WS")
			require.Equal(t, 1, rc.dialer.DialCount())
			require.Equal(t, excelBPSHTTPFallbackReason, rec.Header().Get("X-Codex2API-Basispoints-Bypass"))
		})
	}
}

func TestTiboRouteEnforceUsesBPSVerdict(t *testing.T) {
	for _, mode := range []string{config.OpenAITiboBPSProbeShadow, config.OpenAITiboBPSProbeEnforce} {
		t.Run(mode, func(t *testing.T) {
			tc := newTiboRouteCase(t, true, true, tiboRouteBPSResponse())
			tc.svc.cfg.Gateway.OpenAITiboRoute.BPSProbeMode = mode
			tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
			tc.seed(openAITiboRouteBPS, openAITiboDegraded)
			result, _, _, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
			require.NoError(t, err)
			if mode == config.OpenAITiboBPSProbeShadow {
				require.Equal(t, "/basispoints/api/responses", result.UpstreamEndpoint, "shadow keeps BPS in place")
				return
			}
			require.True(t, result.OpenAIWSMode, "enforce: degraded BPS drops below the healthy Cookie WS")
			require.Empty(t, tc.hosts())
		})
	}
}

func TestCookieWSAllSlotsBusyFallsBackToHTTP(t *testing.T) {
	svc, account, ticket, dialer := newCookieForwardFixture(t, &openAIWSCaptureConn{events: [][]byte{[]byte(tiboRouteWSCompleted)}})
	svc.cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 1
	for slot := 1; slot < openAICookieWSSlotCount; slot++ {
		copied := *ticket
		copied.Slot, copied.Generation = slot, ticket.Generation+"-"+strconv.Itoa(slot)
		svc.openaiCookieWSTickets.Store(openAICookieWSKeySlot(account.ID, ticket.Model, slot), &copied)
	}
	for slot := 0; slot < openAICookieWSSlotCount; slot++ {
		_, release, err := svc.reserveOpenAICookieWSSlot(context.Background(), account, ticket.Model, "")
		require.NoError(t, err)
		t.Cleanup(release)
	}
	upstream := mustTestValue[*httpUpstreamRecorder](t, svc.httpUpstream)
	upstream.resp = cookieWSHTTPResponse("http fallback")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, []byte(tiboRouteAstraBody))
	require.NoError(t, err, "busy slots are unavailable, not an error")
	require.False(t, result.OpenAIWSMode)
	require.Zero(t, dialer.DialCount())
	require.Equal(t, openAICookieWSHTTPFallbackReason, c.GetString("openai_ws_transport_reason"))
	require.Contains(t, rec.Body.String(), "http fallback")
}

func TestReserveCookieWSSlotWithinDistinguishesBusyFromCancel(t *testing.T) {
	svc, account, ticket, _ := newCookieForwardFixture(t, &openAIWSCaptureConn{})
	_, release, err := svc.reserveOpenAICookieWSSlot(context.Background(), account, ticket.Model, "")
	require.NoError(t, err)
	defer release()
	_, _, err = svc.reserveOpenAICookieWSSlotWithin(context.Background(), account, ticket.Model, "", 50*time.Millisecond)
	require.ErrorIs(t, err, errOpenAICookieWSSlotBusy)
	require.True(t, isOpenAICookieWSUnavailableError(err))
	require.True(t, shouldOpenAICookieWSHTTPFallback(err))
	require.True(t, isOpenAICookieWSUnavailableError(openAICookieWSUnavailableFailover(err)), "ingress converts busy into a failover")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = svc.reserveOpenAICookieWSSlotWithin(ctx, account, ticket.Model, "", time.Second)
	require.ErrorIs(t, err, context.Canceled, "caller cancellation stays a client cancellation")
	require.False(t, shouldOpenAICookieWSHTTPFallback(err))
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
	tc := newTiboRouteCase(t, true, true)
	tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
	status := tc.svc.openAICookieWSRuntimeStatus(tc.account, openAICodexTicketDefaultModel, time.Now())
	require.Len(t, status.TiboRoutes, 3)
	require.Equal(t, "http", status.TiboRoutes[0].Route)
	require.Equal(t, "degraded", status.TiboRoutes[0].Verdict)
	require.Equal(t, "bps", status.TiboRoutes[1].Route)
	require.Equal(t, "unknown", status.TiboRoutes[1].Verdict)
	require.Equal(t, "cookie_ws", status.TiboRoutes[2].Route)
	require.Equal(t, "healthy", status.TiboRoutes[2].Verdict)
	require.Empty(t, tc.upstream.requests, "the admin view never probes")
}
