package service

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const tiboRouteWSCompleted = `{"type":"response.completed","response":{"id":"resp_ws","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ws ok"}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`

func tiboRouteStatusResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"denied"}}`))}
}

type tiboZeroDialer struct{}

func (tiboZeroDialer) DialCount() int { return 0 }

type tiboRouteCase struct {
	svc      *OpenAIGatewayService
	account  *Account
	upstream *httpUpstreamRecorder
	dialer   tiboZeroDialer
}

type tiboAccountRepo struct {
	AccountRepository
	account *Account
	updates map[int64]map[string]any
}

func (r *tiboAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.account == nil || r.account.ID != id {
		return nil, errOpenAITiboAccountUnavailable
	}
	copy := *r.account
	copy.Extra = maps.Clone(r.account.Extra)
	copy.Credentials = maps.Clone(r.account.Credentials)
	return &copy, nil
}

func (r *tiboAccountRepo) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	if r.account == nil {
		return nil, nil
	}
	copy := *r.account
	copy.Extra = maps.Clone(r.account.Extra)
	copy.Credentials = maps.Clone(r.account.Credentials)
	return []Account{copy}, nil
}

func (r *tiboAccountRepo) UpdateExtra(_ context.Context, id int64, extra map[string]any) error {
	if r.updates == nil {
		r.updates = map[int64]map[string]any{}
	}
	r.updates[id] = extra
	if r.account != nil && r.account.ID == id {
		if r.account.Extra == nil {
			r.account.Extra = map[string]any{}
		}
		for k, v := range extra {
			r.account.Extra[k] = v
		}
	}
	return nil
}

func cookieWSCompletion(model, answer string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
		"model": model, "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": answer}}}},
	}})
	return b
}

func cookieWSHTTPResponse(answer string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, answer)) + "\n\n"))}
}

func newTiboRouteCase(t *testing.T, _, _ bool, responses ...*http.Response) *tiboRouteCase {
	t.Helper()
	gin.SetMode(gin.TestMode)
	upstream := &httpUpstreamRecorder{responses: responses}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, cache: &stubGatewayCache{}, toolCorrector: NewCodexToolCorrector()}
	account := &Account{
		ID: 23141, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 20, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-chatgpt"},
		Extra:       map[string]any{"openai_passthrough": true},
	}
	svc.accountRepo = &tiboAccountRepo{account: account, updates: map[int64]map[string]any{}}
	return &tiboRouteCase{svc: svc, account: account, upstream: upstream}
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
	require.Contains(t, string(body), openAITiboProbePrompt)
	require.Contains(t, string(body), `"model":"gpt-6-astra"`)
	require.Contains(t, string(body), `"effort":"low"`)
}

func TestTiboRouteAppliesForTicketAccountWithoutCookieWS(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("true."), cookieWSHTTPResponse("http ok"))
	tc.svc.cfg.Gateway.OpenAICodexTicket.Mode = "turn_state"
	tc.svc.cfg.Gateway.OpenAICodexTicket.CookieWSAccountIDs = nil
	require.True(t, tc.svc.openAITiboRouteApplies(tc.account))
	result, rec, _ := tc.forward(t)
	require.False(t, result.OpenAIWSMode)
	require.NotNil(t, result.RouteDegraded)
	require.False(t, *result.RouteDegraded)
	require.Contains(t, rec.Body.String(), "http ok")
}

func TestOpenAICodexTicketStatusesOmitsRowsWithoutTiboRoutes(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.Nil(t, (&AccountTestService{}).OpenAICodexTicketStatuses(account, config.OpenAICodexTicketConfig{}, time.Now()))

	tc := newTiboRouteCase(t, false, false)
	got := (&AccountTestService{openaiGatewayService: tc.svc}).OpenAICodexTicketStatuses(tc.account, config.OpenAICodexTicketConfig{}, time.Now())
	require.Len(t, got, 1)
	require.Equal(t, openAICodexTicketDefaultModel, got[0].Model)
	require.Equal(t, "http", got[0].TiboRoutes[0].Route)
}

func TestTiboRouteHTTPTrueStaysOnHTTP(t *testing.T) {
	tc := newTiboRouteCase(t, false, true, cookieWSHTTPResponse("true."), cookieWSHTTPResponse("http ok"))
	result, rec, _ := tc.forward(t)
	require.False(t, result.OpenAIWSMode)
	require.Zero(t, tc.dialer.DialCount(), "healthy HTTP must not use Cookie WS")
	require.Equal(t, []string{"chatgpt.com", "chatgpt.com"}, tc.hosts(), "probe, then business HTTP")
	requireTiboProbe(t, tc.upstream.bodies[0])
	require.Contains(t, string(tc.upstream.bodies[1]), "hello")
	require.False(t, result.OpenAIWSMode)
	require.Contains(t, rec.Body.String(), "http ok")
	require.NotNil(t, result.RouteDegraded)
	require.False(t, *result.RouteDegraded)
	requireNoOpenAIRoutingHeaders(t, rec.Header())
}

func TestTiboRouteDegradedStaysOnHTTP(t *testing.T) {
	tc := newTiboRouteCase(t, false, true, cookieWSHTTPResponse("False"), cookieWSHTTPResponse("http ok"))
	result, rec, _ := tc.forward(t)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, []string{"chatgpt.com", "chatgpt.com"}, tc.hosts())
	require.Zero(t, tc.dialer.DialCount())
	require.Empty(t, tc.upstream.requests[1].Header.Get(openAICodexTurnStateHeader))
	require.Contains(t, rec.Body.String(), "http ok")
	require.NotNil(t, result.RouteDegraded)
	require.True(t, *result.RouteDegraded)
}

func TestTiboRouteEverythingUnusableStillSendsHTTP(t *testing.T) {
	tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("False"), cookieWSHTTPResponse("http ok"))
	result, rec, _ := tc.forward(t)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, []string{"chatgpt.com", "chatgpt.com"}, tc.hosts())
	require.Zero(t, tc.dialer.DialCount())
	require.Contains(t, rec.Body.String(), "http ok")
	require.NotNil(t, result.RouteDegraded)
	require.True(t, *result.RouteDegraded, "only degraded routes remain; recorded in usage, not headers")
	requireNoOpenAIRoutingHeaders(t, rec.Header())
}

// A cold account whose first probe is unknown has no confirmed verdict, so it
// still sends HTTP, and unknown does not become a verdict.
func TestTiboRouteProbeFailureIsNotHealthy(t *testing.T) {
	tc := newTiboRouteCase(t, false, true, tiboRouteStatusResponse(http.StatusTooManyRequests), cookieWSHTTPResponse("http ok"))
	result, rec, _ := tc.forward(t)
	require.False(t, result.OpenAIWSMode)
	require.Contains(t, rec.Body.String(), "http ok")
	state := tc.svc.loadOpenAITiboAccountState(tc.account.ID)
	require.NotNil(t, state)
	require.Empty(t, state.http.verdict, "unknown samples never become a verdict")
	require.Equal(t, 1, state.http.unknownStreak)
	require.Equal(t, openAITiboUnknown, tc.svc.openAITiboEffective(tc.account.ID, openAITiboRouteHTTP, time.Now()))
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
	if strings.Contains(string(body), openAITiboProbePrompt) {
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
	require.Equal(t, "", tc.upstream.lastProxyURL)
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
