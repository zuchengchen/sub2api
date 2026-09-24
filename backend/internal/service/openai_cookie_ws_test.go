package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func cookieWSTestTicket(accountID int64, captured time.Time) *openAICookieWSTicket {
	return &openAICookieWSTicket{
		AccountID: accountID, Model: openAICodexTicketDefaultModel, Generation: "generation-current", Cookies: openAICodexTicketTestCookies,
		Identity: newOpenAICookieWSIdentity(), CapturedAt: captured, RefreshAt: captured.Add(openAICookieWSRefreshAge),
		ExpiresAt: captured.Add(openAICookieWSLifetime), HTTPVerified: true, WSVerified: true, processVerified: true,
	}
}

func cookieWSCompletion(model, answer string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
		"model": model, "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": answer}}}},
	}})
	return b
}

func cookieWSHTTPResponse(answer string) *http.Response {
	h := http.Header{}
	stampCodexTicketSetCookies(h)
	h.Set(openAICodexTurnStateHeader, fakeCodexTicketState(780))
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, answer)) + "\n\n"))}
}

type cookieWSLifecycleRepo struct {
	AccountRepository
	mu       sync.Mutex
	accounts []Account
	updates  map[int64]map[string]any
	err      error
}

func (r *cookieWSLifecycleRepo) ListByPlatform(context.Context, string) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := append([]Account(nil), r.accounts...)
	for i := range result {
		result[i].Extra = maps.Clone(result[i].Extra)
		result[i].Credentials = maps.Clone(result[i].Credentials)
	}
	return result, nil
}

func (r *cookieWSLifecycleRepo) UpdateExtra(_ context.Context, accountID int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if r.updates == nil {
		r.updates = make(map[int64]map[string]any)
	}
	if r.updates[accountID] == nil {
		r.updates[accountID] = make(map[string]any)
	}
	maps.Copy(r.updates[accountID], updates)
	return nil
}

type cookieWSProbeConn struct {
	answers   [][]byte
	writes    []map[string]any
	closed    atomic.Bool
	pingCount atomic.Int32
}

func (c *cookieWSProbeConn) WriteJSON(_ context.Context, v any) error {
	c.writes = append(c.writes, v.(map[string]any))
	return nil
}
func (c *cookieWSProbeConn) ReadMessage(context.Context) ([]byte, error) {
	if len(c.answers) == 0 {
		return nil, io.EOF
	}
	answer := c.answers[0]
	c.answers = c.answers[1:]
	return answer, nil
}
func (c *cookieWSProbeConn) Ping(context.Context) error { c.pingCount.Add(1); return nil }
func (c *cookieWSProbeConn) Close() error               { c.closed.Store(true); return nil }

type cookieWSProbeDialer struct {
	conn    *cookieWSProbeConn
	headers http.Header
	proxy   string
	dials   int
}

func (d *cookieWSProbeDialer) Dial(_ context.Context, _ string, h http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	d.headers, d.proxy = h.Clone(), proxy
	d.dials++
	return d.conn, 0, http.Header{}, nil
}

func cookieWSTestService(t *testing.T, upstream HTTPUpstream, answers ...string) (*OpenAIGatewayService, *cookieWSProbeDialer) {
	t.Helper()
	s := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled: true, Mode: openAICookieWSMode, HarvestProxyURL: "socks5h://dynamic.example:1080", HarvestAttemptTimeoutSeconds: 1,
	}, upstream)
	conn := &cookieWSProbeConn{}
	for _, answer := range answers {
		conn.answers = append(conn.answers, cookieWSCompletion(openAICodexTicketDefaultModel, answer))
	}
	dialer := &cookieWSProbeDialer{conn: conn}
	s.cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 32
	s.cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 32
	s.cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	s.cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 1
	s.openaiWSPool = newOpenAIWSConnPool(s.cfg)
	s.openaiWSPool.setClientDialerForTest(dialer)
	t.Cleanup(s.openaiWSPool.Close)
	return s, dialer
}

func TestOpenAICookieWSLifecycleClockBoundaries(t *testing.T) {
	captured := time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC)
	ticket := cookieWSTestTicket(41, captured)
	require.True(t, ticket.ready(captured.Add(49*time.Minute+59*time.Second)))
	require.False(t, ticket.needsRefresh(captured.Add(49*time.Minute+59*time.Second)))
	require.True(t, ticket.needsRefresh(captured.Add(50*time.Minute)))
	require.True(t, ticket.ready(captured.Add(59*time.Minute+59*time.Second)))
	require.False(t, ticket.ready(captured.Add(60*time.Minute)))
	ticket.ExpiresAt = captured.Add(2 * time.Hour)
	require.False(t, ticket.ready(captured.Add(60*time.Minute)), "persisted expiry cannot extend lifetime")
	require.False(t, ticket.ready(captured.Add(-time.Second)), "future capture rejected")
}

func TestOpenAICookieWSProbeRequiresExactSuccessfulTrue(t *testing.T) {
	tests := []struct {
		name, body string
		passed     bool
	}{
		{"true", "data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, "True")) + "\n\n", true},
		{"false", "data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, "False")) + "\n\n", false},
		{"wrong model", "data: " + string(cookieWSCompletion("gpt-5.6-luna", "True")) + "\n\n", false},
		{"explanation", "data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, "True because")) + "\n\n", false},
		{"truncated", `data: {"type":"response.output_text.delta","delta":"True"}` + "\n\n", false},
		{"failed first", `data: {"type":"response.failed"}` + "\n\ndata: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, "True")) + "\n\n", false},
		{"duplicate completed", "data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, "True")) + "\n\ndata: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, "True")) + "\n\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { require.Equal(t, tt.passed, openAICookieWSProbePassed([]byte(tt.body))) })
	}
}

func TestOpenAICookieWSHarvestAndVerifyBeforePublish(t *testing.T) {
	u := &httpUpstreamRecorder{resp: cookieWSHTTPResponse("True")}
	s, dialer := cookieWSTestService(t, u, "True", "True")
	account := ticketTestAccount(41)
	s.refreshOpenAICookieWSAccount(context.Background(), account)
	ticket := s.lookupOpenAICookieWSTicket(account, openAICodexTicketDefaultModel)
	require.True(t, ticket.ready(time.Now()))
	require.Equal(t, openAICookieWSLifetime, ticket.ExpiresAt.Sub(ticket.CapturedAt))
	require.Equal(t, openAICookieWSRefreshAge, ticket.RefreshAt.Sub(ticket.CapturedAt))
	require.Equal(t, "socks5h://dynamic.example:1080", u.lastProxyURL)
	require.True(t, u.lastReq.Close)
	require.Empty(t, u.lastReq.Header.Get("Cookie"))
	require.Empty(t, u.lastReq.Header.Get(openAICodexTurnStateHeader))
	require.Equal(t, "low", gjson.GetBytes(u.lastBody, "reasoning.effort").String())
	require.Equal(t, "", gjson.GetBytes(u.lastBody, "instructions").String())
	require.Equal(t, openAICookieWSProbePrompt, gjson.GetBytes(u.lastBody, "input.0.content.0.text").String())
	require.False(t, gjson.GetBytes(u.lastBody, "tools").Exists())
	require.Equal(t, "", dialer.proxy, "WS must bypass account proxy")
	require.Equal(t, ticket.Cookies, dialer.headers.Get("Cookie"))
	require.Empty(t, dialer.headers.Get(openAICodexTurnStateHeader))
	require.Equal(t, u.lastReq.Header.Get("session_id"), dialer.headers.Get("session_id"))
	require.Len(t, dialer.conn.writes, 2)
	for _, payload := range dialer.conn.writes {
		require.Equal(t, "response.create", payload["type"])
		require.NotContains(t, payload, "previous_response_id")
		require.NotContains(t, payload, "stream")
	}
	require.Zero(t, dialer.conn.pingCount.Load())
	require.True(t, dialer.conn.closed.Load(), "validation socket is retired after publication")
	s.refreshOpenAICookieWSAccount(context.Background(), account)
	require.Len(t, u.requests, 1, "before 50 minutes do not refresh")
}

func TestOpenAICookieWSRefreshFailureKeepsOldGeneration(t *testing.T) {
	for _, failure := range []string{"http_false", "second_ws_false", "persistence"} {
		t.Run(failure, func(t *testing.T) {
			answer := "True"
			if failure == "http_false" {
				answer = "False"
			}
			u := &httpUpstreamRecorder{resp: cookieWSHTTPResponse(answer)}
			wsAnswer := "True"
			if failure == "second_ws_false" {
				wsAnswer = "False"
			}
			s, dialer := cookieWSTestService(t, u, "True", wsAnswer)
			account := ticketTestAccount(41)
			old := cookieWSTestTicket(account.ID, time.Now().Add(-51*time.Minute))
			s.openaiCookieWSTickets.Store(openAICodexTicketKey(account.ID, old.Model), old)
			if failure == "persistence" {
				s.accountRepo = &cookieWSLifecycleRepo{err: errors.New("unavailable")}
			}
			s.refreshOpenAICookieWSAccount(context.Background(), account)
			require.Same(t, old, s.lookupOpenAICookieWSTicket(account, old.Model))
			require.True(t, old.ready(time.Now()))
			require.True(t, s.openAICookieWSRetryWaiting(account.ID, time.Now()))
			if failure != "http_false" {
				require.True(t, dialer.conn.closed.Load())
			}
			require.Equal(t, old.Identity.SessionID, u.lastReq.Header.Get("session_id"), "identity survives refresh")
		})
	}
}

func TestOpenAICookieWSIsolationExpiryAndRestartGate(t *testing.T) {
	s, _ := cookieWSTestService(t, nil)
	a, b := ticketTestAccount(41), ticketTestAccount(42)
	ticket := cookieWSTestTicket(a.ID, time.Now())
	s.openaiCookieWSTickets.Store(openAICodexTicketKey(a.ID, ticket.Model), ticket)
	h := http.Header{openAICodexTurnStateHeader: []string{"client-state"}}
	require.NoError(t, s.applyOpenAICookieWSHeaders(context.Background(), a, ticket.Model, h))
	require.Equal(t, ticket.Generation, h.Get(openAICookieWSGenerationHeader))
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
	require.ErrorIs(t, s.applyOpenAICookieWSHeaders(context.Background(), b, ticket.Model, http.Header{}), ErrOpenAICodexTicketUnavailable)
	require.True(t, s.openAICodexTicketBlocksAccount(b, ticket.Model), "cookie mode is fail closed even with legacy FailClosed=false")
	require.False(t, s.openAICodexTicketBlocksAccount(a, ticket.Model))
	parsed := parseOpenAICookieWSTicket(a.ID, ticket.Model, ticket)
	require.False(t, parsed.ready(time.Now()), "restart cannot trust stale process validation")
	require.True(t, parsed.valid(time.Now()))
	require.Nil(t, parseOpenAICookieWSTicket(b.ID, ticket.Model, ticket), "no cross-account adoption")
	old := cookieWSTestTicket(a.ID, time.Now().Add(-time.Hour))
	s.openaiCookieWSTickets.Store(openAICodexTicketKey(a.ID, old.Model), old)
	require.ErrorIs(t, s.applyOpenAICookieWSHeaders(context.Background(), a, old.Model, http.Header{}), ErrOpenAICodexTicketUnavailable)
}

func TestOpenAICookieWSRestoreRevalidatesWithoutExtendingLifetime(t *testing.T) {
	s, dialer := cookieWSTestService(t, nil, "True", "True")
	account := ticketTestAccount(41)
	stored := cookieWSTestTicket(account.ID, time.Now().Add(-20*time.Minute))
	account.Extra = map[string]any{openAICookieWSExtraKey(stored.Model): stored}
	require.True(t, s.openAICodexTicketBlocksAccount(account, stored.Model))
	s.refreshOpenAICookieWSAccount(context.Background(), account)
	got := s.lookupOpenAICookieWSTicket(account, stored.Model)
	require.True(t, got.ready(time.Now()))
	require.True(t, stored.CapturedAt.Equal(got.CapturedAt))
	require.True(t, stored.ExpiresAt.Equal(got.ExpiresAt))
	require.Equal(t, stored.Generation, got.Generation)
	require.Equal(t, 1, dialer.dials)
}

func TestOpenAICookieWSBackoffHonorsRetryAfter(t *testing.T) {
	s, _ := cookieWSTestService(t, nil)
	now := time.Now()
	for _, delay := range []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute, time.Minute} {
		s.noteOpenAICookieWSMiss(41, now, "http_false", 200, nil)
		raw, _ := s.openaiCookieWSRetry.Load(openAICodexTicketKey(41, openAICodexTicketDefaultModel))
		require.Equal(t, now.Add(delay), raw.(*openAICookieWSRetryState).nextAttemptAt)
	}
	s.noteOpenAICookieWSMiss(41, now, "quota", 429, http.Header{"Retry-After": []string{"180"}})
	raw, _ := s.openaiCookieWSRetry.Load(openAICodexTicketKey(41, openAICodexTicketDefaultModel))
	require.Equal(t, now.Add(180*time.Second), raw.(*openAICookieWSRetryState).nextAttemptAt)
}

func TestOpenAICookieWSPrivateExtraCannotBeExportedOrOverwritten(t *testing.T) {
	key := openAICookieWSExtraKey(openAICodexTicketDefaultModel)
	current := map[string]any{key: map[string]any{"cookies": "private", "generation": "persisted"}}
	redacted := RedactOpenAICodexTicketExtra(current)
	require.NotContains(t, redacted, key)
	merged := MergeOpenAICodexTicketExtra(map[string]any{key: "spoofed", "setting": true}, current)
	require.Equal(t, current[key], merged[key])
	require.True(t, merged["setting"].(bool))
}

func TestOpenAICookieWSRolloutScopeAndMetadata(t *testing.T) {
	s, _ := cookieWSTestService(t, nil)
	s.cfg.Gateway.OpenAICodexTicket.Mode = " COOKIE_WS "
	s.cfg.Gateway.OpenAICodexTicket.CookieWSAccountIDs = []int64{41}
	require.True(t, s.openAICookieWSEnabledForModel(ticketTestAccount(41), openAICodexTicketDefaultModel))
	require.False(t, s.openAICookieWSEnabledForModel(ticketTestAccount(42), openAICodexTicketDefaultModel))
	require.False(t, s.openAICookieWSEnabledForModel(ticketTestAccount(41), openAICodexTicketDefaultSolModel))
	ticket := cookieWSTestTicket(41, time.Now())
	h := http.Header{}
	applyOpenAICookieWSTicketHeaders(h, ticket)
	payload := map[string]any{"client_metadata": map[string]any{"session_id": "untrusted", "turn_id": "preserve"}}
	applyOpenAICookieWSMetadataFromHeaders(h, payload)
	require.Equal(t, ticket.Identity.SessionID, payload["client_metadata"].(map[string]any)["session_id"])
	require.Equal(t, "preserve", payload["client_metadata"].(map[string]any)["turn_id"])
	empty := map[string]any{}
	applyOpenAICookieWSMetadataFromHeaders(h, empty)
	require.NotContains(t, empty, "client_metadata")
}

func TestOpenAICookieWSModeLeavesNonselectedTrafficAndLegacyHarvesterInactive(t *testing.T) {
	u := &httpUpstreamRecorder{resp: cookieWSHTTPResponse("True")}
	s, _ := cookieWSTestService(t, u)
	s.cfg.Gateway.OpenAICodexTicket.CookieWSAccountIDs = []int64{41}
	s.cfg.Gateway.OpenAICodexTicket.FailClosed = true
	selected, other := ticketTestAccount(41), ticketTestAccount(42)
	s.accountRepo = &cookieWSLifecycleRepo{accounts: []Account{*selected, *other}}
	for _, tc := range []struct {
		account *Account
		model   string
	}{{other, openAICodexTicketDefaultModel}, {selected, openAICodexTicketDefaultSolModel}} {
		h := http.Header{}
		h.Set(openAICodexTurnStateHeader, "client-state")
		require.NoError(t, s.applyOpenAICodexTicket(context.Background(), tc.account, tc.model, h))
		require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
		require.False(t, s.openAICodexTicketBlocksAccount(tc.account, tc.model))
	}
	require.Nil(t, OpenAICodexTicketStatuses(other, s.openAICodexTicketConfig(), time.Now()))
	status := OpenAICodexTicketStatuses(selected, s.openAICodexTicketConfig(), time.Now())
	require.Len(t, status, 1)
	require.Equal(t, openAICookieWSMode, status[0].Mode)
	require.True(t, status[0].Blocked)
	s.refreshOpenAICodexTickets(context.Background())
	s.probeOnceOpenAICodexTicket(context.Background(), other, openAICodexTicketDefaultModel)
	require.Empty(t, u.requests)
}

func TestOpenAICookieWSConcurrentRefreshSingleflight(t *testing.T) {
	u := &httpUpstreamRecorder{resp: cookieWSHTTPResponse("True")}
	s, dialer := cookieWSTestService(t, u, "True", "True")
	account := ticketTestAccount(41)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); s.refreshOpenAICookieWSAccount(context.Background(), account) }()
	}
	wg.Wait()
	require.Len(t, u.requests, 1)
	require.Equal(t, 1, dialer.dials)
	require.True(t, s.lookupOpenAICookieWSTicket(account, openAICodexTicketDefaultModel).ready(time.Now()))
}

func TestOpenAICookieWSAdminTestsUseVerifiedDirectPool(t *testing.T) {
	for _, intelligent := range []bool{false, true} {
		t.Run(map[bool]string{false: "connection", true: "intelligence"}[intelligent], func(t *testing.T) {
			u := &httpUpstreamRecorder{err: errors.New("HTTP business fallback forbidden")}
			gateway, dialer := cookieWSTestService(t, u, "True")
			account := ticketTestAccount(41)
			ticket := cookieWSTestTicket(account.ID, time.Now())
			gateway.openaiCookieWSTickets.Store(openAICodexTicketKey(account.ID, ticket.Model), ticket)
			svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: u}
			ctx := context.Background()
			capture := &intelligentCapture{}
			if intelligent {
				ctx = context.WithValue(ctx, intelligentRunKey{}, &intelligentRunContext{prompt: openAICookieWSProbePrompt, testType: "tibo", capture: capture})
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/admin/accounts/41/test", nil).WithContext(ctx)
			require.NoError(t, svc.testOpenAIAccountConnection(c, account, ticket.Model, "hello", AccountTestModeDefault))
			require.Empty(t, u.requests)
			require.Equal(t, "", dialer.proxy)
			require.Equal(t, ticket.Cookies, dialer.headers.Get("Cookie"))
			require.Contains(t, recorder.Body.String(), `"type":"content","text":"True"`)
			require.Contains(t, recorder.Body.String(), `"type":"test_complete"`)
			require.NotContains(t, recorder.Body.String(), ticket.Cookies)
			if intelligent {
				require.True(t, capture.upstreamComplete())
				require.Equal(t, http.StatusOK, capture.status)
			}
		})
	}
}

func TestOpenAICookieWSAdminMissingCookieNeverUsesHTTP(t *testing.T) {
	u := &httpUpstreamRecorder{err: errors.New("HTTP business fallback forbidden")}
	gateway, dialer := cookieWSTestService(t, u)
	svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: u}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/accounts/41/test", nil)
	require.Error(t, svc.testOpenAIAccountConnection(c, ticketTestAccount(41), openAICodexTicketDefaultModel, "", AccountTestModeDefault))
	require.Empty(t, u.requests)
	require.Zero(t, dialer.dials)
	require.Contains(t, recorder.Body.String(), "No verified Cookie websocket")
}

func TestOpenAICookieWSTwoSlotsAreIndependentAndEitherReadySchedules(t *testing.T) {
	s, _ := cookieWSTestService(t, nil)
	account := ticketTestAccount(41)
	first := cookieWSTestTicket(account.ID, time.Now().Add(-time.Hour))
	second := cookieWSTestTicket(account.ID, time.Now().Add(-time.Minute))
	second.Slot = 1
	second.Generation = "slot-one"
	second.Cookies = "independent=second"
	s.openaiCookieWSTickets.Store(openAICookieWSKeySlot(account.ID, first.Model, 0), first)
	s.openaiCookieWSTickets.Store(openAICookieWSKeySlot(account.ID, second.Model, 1), second)
	require.Same(t, second, s.lookupOpenAICookieWSTicket(account, second.Model))
	require.False(t, s.openAICodexTicketBlocksAccount(account, second.Model))
	h := http.Header{}
	require.NoError(t, s.applyOpenAICookieWSHeadersForSlot(context.Background(), account, second.Model, h, 1))
	require.Equal(t, "1", h.Get(openAICookieWSSlotHeader))
	require.Equal(t, second.Cookies, h.Get("Cookie"))
	require.Error(t, s.applyOpenAICookieWSHeadersForSlot(context.Background(), account, second.Model, http.Header{}, 0))
	s.noteOpenAICookieWSSlotMiss(account.ID, 0, time.Now(), "probe_failed", 500, nil)
	require.True(t, s.openAICookieWSSlotRetryWaiting(account.ID, 0, time.Now()))
	require.False(t, s.openAICookieWSSlotRetryWaiting(account.ID, 1, time.Now()))
	account.Extra = map[string]any{openAICookieWSExtraKeySlot(first.Model, 0): first, openAICookieWSExtraKeySlot(second.Model, 1): second}
	status := openAICookieWSStatus(account, second.Model, time.Now())
	require.Equal(t, 1, status.CookieGroupsReady)
	require.Equal(t, 2, status.CookieGroupsTotal)
	require.Equal(t, 10, status.WSPerGroup)
	require.True(t, status.Ready)
	require.NotContains(t, RedactOpenAICodexTicketExtra(account.Extra), openAICookieWSExtraKeySlot(second.Model, 1))
	require.NotEqual(t, first.Identity.SessionID, second.Identity.SessionID)
}

func TestOpenAICookieWSRefreshSecondSlotKeepsFirst(t *testing.T) {
	u := &httpUpstreamRecorder{resp: cookieWSHTTPResponse("True")}
	s, _ := cookieWSTestService(t, u, "True", "True")
	account := ticketTestAccount(41)
	first := cookieWSTestTicket(account.ID, time.Now())
	s.openaiCookieWSTickets.Store(openAICookieWSKeySlot(account.ID, first.Model, 0), first)
	s.refreshOpenAICookieWSSlot(context.Background(), account, 1)
	second := s.lookupOpenAICookieWSTicketSlot(account, first.Model, 1)
	require.True(t, second.ready(time.Now()))
	require.Equal(t, 1, second.Slot)
	require.Same(t, first, s.lookupOpenAICookieWSTicketSlot(account, first.Model, 0))
	require.NotEqual(t, first.Identity, second.Identity)
	require.NotEqual(t, first.Generation, second.Generation)
}

func TestOpenAICookieWSHTTPMissDiagnosticsDistinguishStatusCookieAndAnswer(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		noCookie       bool
		answer, reason string
	}{
		{"status", 403, false, "True", "http_probe_status"},
		{"cookie", 200, true, "True", "http_probe_missing_cookie"},
		{"answer", 200, false, "False", "http_probe_not_true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := cookieWSHTTPResponse(tc.answer)
			resp.StatusCode = tc.status
			if tc.noCookie {
				resp.Header.Del("Set-Cookie")
			}
			s, _ := cookieWSTestService(t, &httpUpstreamRecorder{resp: resp})
			s.refreshOpenAICookieWSSlot(context.Background(), ticketTestAccount(41), 1)
			raw, ok := s.openaiCookieWSRetry.Load(openAICookieWSKeySlot(41, openAICodexTicketDefaultModel, 1))
			require.True(t, ok)
			require.Equal(t, tc.reason, raw.(*openAICookieWSRetryState).reason)
		})
	}
}
