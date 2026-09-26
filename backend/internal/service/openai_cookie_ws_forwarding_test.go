package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type cookieForwardDialer struct {
	openAIWSCaptureDialer
	proxy string
}

func (d *cookieForwardDialer) Dial(ctx context.Context, wsURL string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	d.proxy = proxy
	return d.openAIWSCaptureDialer.Dial(ctx, wsURL, headers, proxy)
}

func newCookieForwardFixture(t *testing.T, conn *openAIWSCaptureConn) (*OpenAIGatewayService, *Account, *openAICookieWSTicket, *cookieForwardDialer) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Gateway.OpenAICodexTicket.Enabled = true
	cfg.Gateway.OpenAICodexTicket.Mode = openAICookieWSMode
	cfg.Gateway.OpenAICodexTicket.CookieWSAccountIDs = []int64{23141}
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 24
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 24
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 2
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	pool := newOpenAIWSConnPool(cfg)
	pool.SetCookieValidator(func(context.Context, *Account, *openAIWSConnLease) error { return nil })
	t.Cleanup(pool.Close)
	dialer := &cookieForwardDialer{openAIWSCaptureDialer: openAIWSCaptureDialer{conn: conn}}
	pool.setClientDialerForTest(dialer)
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{}, openaiWSPool: pool, toolCorrector: NewCodexToolCorrector()}
	proxyID := int64(1)
	account := &Account{ID: 23141, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 20, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-chatgpt", "model_mapping": map[string]any{"astra-alias": "gpt-6-astra"}},
		Extra:       map[string]any{"openai_passthrough": true, "openai_oauth_responses_websockets_v2_mode": "off"},
		ProxyID:     &proxyID, Proxy: &Proxy{ID: 1, Protocol: "socks5", Host: "bound-proxy.invalid", Port: 1080}}
	svc.accountRepo = &cookieWSLifecycleRepo{accounts: []Account{*account}}
	now := time.Now().Add(-time.Minute)
	ticket := &openAICookieWSTicket{AccountID: account.ID, Model: "gpt-6-astra", Generation: "generation-one", Cookies: "__oailb=test; __cf_bm=test; __cflb=test", Identity: newOpenAICookieWSIdentity(), CapturedAt: now, RefreshAt: now.Add(50 * time.Minute), ExpiresAt: now.Add(time.Hour), HTTPVerified: true, WSVerified: true, processVerified: true}
	svc.openaiCookieWSTickets.Store(openAICodexTicketKey(account.ID, ticket.Model), ticket)
	return svc, account, ticket, dialer
}

func TestCookieWSForwardHTTPRoutingAndFinalIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		stream       bool
		instructions string
	}{{false, ""}, {true, ""}, {false, "Keep my instructions exactly."}} {
		t.Run(fmt.Sprintf("stream_%v_custom_%v", tc.stream, tc.instructions != ""), func(t *testing.T) {
			conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_cookie","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"True"}]}],"usage":{"input_tokens":33,"output_tokens":12}}}`)}}
			svc, account, ticket, dialer := newCookieForwardFixture(t, conn)
			svc.cfg.Gateway.ForceCodexCLI = true
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("User-Agent", "client-identity")
			c.Request.Header.Set("thread-id", "client-thread")
			c.Request.Header.Set(openAIWSTurnStateHeader, "untrusted-old-state")
			c.Set("api_key", &APIKey{ID: 77})
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"instructions":%q,"input":"hello","client_metadata":{"session_id":"old","x-codex-installation-id":"old","x-codex-window-id":"old"}}`, tc.stream, tc.instructions))
			result, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.OpenAIWSMode)
			require.Equal(t, 33, result.Usage.InputTokens)
			require.Equal(t, 12, result.Usage.OutputTokens)
			require.Equal(t, "", dialer.proxy, "bound account proxy must be bypassed")
			require.Equal(t, ticket.Cookies, dialer.lastHeaders.Get("Cookie"))
			require.Equal(t, ticket.Identity.UserAgent, dialer.lastHeaders.Get("User-Agent"))
			require.Equal(t, ticket.Identity.SessionID, dialer.lastHeaders.Get("session_id"))
			require.Equal(t, ticket.Identity.ThreadID, dialer.lastHeaders.Get("thread-id"))
			require.Empty(t, dialer.lastHeaders.Get(openAIWSTurnStateHeader))
			require.Empty(t, dialer.lastHeaders.Get("x-codex-beta-features"))
			require.Equal(t, "gpt-6-astra", conn.lastWrite["model"])
			require.Equal(t, tc.instructions, conn.lastWrite["instructions"])
			require.NotContains(t, conn.lastWrite, "stream")
			metadata := conn.lastWrite["client_metadata"].(map[string]any)
			require.Equal(t, ticket.Identity.SessionID, metadata["session_id"])
			require.Equal(t, ticket.Identity.InstallationID, metadata["x-codex-installation-id"])
			require.Equal(t, ticket.Identity.WindowID, metadata["x-codex-window-id"])
			require.Empty(t, svc.httpUpstream.(*httpUpstreamRecorder).lastBody)
			require.Contains(t, rec.Body.String(), "True")
		})
	}
}

func TestCookieWSMissingCookieDoesNotBlockScheduling(t *testing.T) {
	svc, account, ticket, _ := newCookieForwardFixture(t, &openAIWSCaptureConn{})
	require.False(t, svc.openAICodexTicketBlocksAccount(account, ticket.Model))
	svc.openaiCookieWSTickets.Delete(openAICodexTicketKey(account.ID, ticket.Model))
	require.False(t, svc.openAICodexTicketBlocksAccount(account, ticket.Model))
	require.False(t, svc.isOpenAIAccountRequestRuntimeBlocked(account, ticket.Model, false))
	h := http.Header{}
	require.NoError(t, svc.applyOpenAICodexTicket(context.Background(), account, ticket.Model, h))
	require.Empty(t, h.Get(openAICodexTurnStateHeader))
}

func TestCookieWSForwardMissingCookieUsesHTTPResponses(t *testing.T) {
	svc, account, _, dialer := newCookieForwardFixture(t, &openAIWSCaptureConn{})
	svc.openaiCookieWSTickets.Delete(openAICodexTicketKey(account.ID, "gpt-6-astra"))
	upstream := svc.httpUpstream.(*httpUpstreamRecorder)
	upstream.resp = cookieWSHTTPResponse("HTTP fallback")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	_, passthrough := c.Get("openai_passthrough")
	require.False(t, passthrough, "missing Cookie WS must use the normal Responses path")
	require.Zero(t, dialer.DialCount())
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
	require.Empty(t, upstream.lastReq.Header.Get("Cookie"))
	require.Contains(t, rec.Body.String(), "HTTP fallback")
}

func TestCookieWSRouteScopeAndLegacyBoundaries(t *testing.T) {
	svc, account, ticket, _ := newCookieForwardFixture(t, &openAIWSCaptureConn{})
	account.Extra["openai_passthrough"] = false
	legacy := openAIWSHTTPDecision("client_protocol_http")
	decision, enabled := svc.resolveOpenAICookieWSDecision(account, "astra-alias", false, legacy)
	require.True(t, enabled)
	require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2, decision.Transport)
	svc.openaiCookieWSTickets.Delete(openAICodexTicketKey(account.ID, ticket.Model))
	decision, enabled = svc.resolveOpenAICookieWSDecision(account, "astra-alias", false, legacy)
	require.False(t, enabled)
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, decision.Transport)
	require.Equal(t, openAICookieWSHTTPFallbackReason, decision.Reason)
	svc.openaiCookieWSTickets.Store(openAICodexTicketKey(account.ID, ticket.Model), ticket)
	decision, enabled = svc.resolveOpenAICookieWSDecision(account, "gpt-6-sol", false, legacy)
	require.False(t, enabled)
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, decision.Transport)
	decision, enabled = svc.resolveOpenAICookieWSDecision(account, "gpt-6-astra", true, legacy)
	require.False(t, enabled)
	require.Equal(t, legacy, decision)
	require.True(t, svc.isOpenAIAccountTransportCompatible(account, OpenAIUpstreamTransportResponsesWebsocketV2Ingress, "astra-alias"))
	account.Extra["openai_passthrough"] = true
	require.True(t, svc.isOpenAIAccountTransportCompatible(account, OpenAIUpstreamTransportResponsesWebsocketV2Ingress, "astra-alias"))
	decision, enabled = svc.resolveOpenAICookieWSDecision(account, "astra-alias", false, legacy)
	require.False(t, enabled, "HTTP passthrough leaves an alias unmapped")
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, decision.Transport)
	require.True(t, svc.isOpenAIAccountTransportCompatible(account, OpenAIUpstreamTransportResponsesWebsocketV2Ingress, "gpt-6-sol"), "non-Astra WS clients use the HTTP Responses bridge")
	require.False(t, svc.isOpenAIAccountTransportCompatible(account, OpenAIUpstreamTransportResponsesWebsocketV2, "gpt-6-sol"))
	svc.cfg.Gateway.OpenAICodexTicket.Mode = "turn_state"
	decision, enabled = svc.resolveOpenAICookieWSDecision(account, "gpt-6-astra", false, legacy)
	require.False(t, enabled)
	require.Equal(t, legacy, decision)
	h1, h2 := make(http.Header), make(http.Header)
	c1, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c1.Set("api_key", &APIKey{ID: 1})
	c2.Set("api_key", &APIKey{ID: 2})
	setOpenAICookieWSExecutionScope(h1, c1, "same-thread")
	setOpenAICookieWSExecutionScope(h2, c2, "same-thread")
	require.NotEqual(t, h1.Get(openAICookieWSScopeHeader), h2.Get(openAICookieWSScopeHeader))
	setOpenAICookieWSExecutionScope(h2, c1, "other-thread")
	require.NotEqual(t, h1.Get(openAICookieWSScopeHeader), h2.Get(openAICookieWSScopeHeader))
}

func TestCookieWSIngressOverridesLegacyHTTPBridge(t *testing.T) {
	conn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.completed","response":{"id":"resp_cookie_ingress_1","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_cookie_ingress_2","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`),
	}}
	svc, account, ticket, dialer := newCookieForwardFixture(t, conn)
	svc.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	svc.cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeHTTPBridge
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer ws.CloseNow()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		c.Set("api_key", &APIKey{ID: 88})
		_, first, err := ws.Read(r.Context())
		if err != nil {
			serverErrors <- err
			return
		}
		hooks := &OpenAIWSIngressHooks{MapRequestModel: func(int, string) (string, error) { return "gpt-6-astra", nil }}
		serverErrors <- svc.ProxyResponsesWebSocketFromClient(r.Context(), c, ws, account, "test-token", first, hooks)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	for i := 1; i <= 2; i++ {
		require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"channel-alias","input":"hello","client_metadata":{"session_id":"source"}}`)))
		_, event, readErr := client.Read(ctx)
		require.NoError(t, readErr)
		require.Equal(t, fmt.Sprintf("resp_cookie_ingress_%d", i), gjson.GetBytes(event, "response.id").String())
	}
	_ = client.Close(coderws.StatusNormalClosure, "done")
	select {
	case err := <-serverErrors:
		require.True(t, err == nil || errors.Is(err, context.Canceled), "%v", err)
	case <-ctx.Done():
		t.Fatal("Cookie ingress did not close")
	}
	require.Equal(t, 1, dialer.DialCount())
	require.Equal(t, "", dialer.proxy)
	require.Equal(t, ticket.Cookies, dialer.lastHeaders.Get("Cookie"))
	require.Empty(t, svc.httpUpstream.(*httpUpstreamRecorder).lastBody)
	require.Len(t, conn.writes, 2)
	for _, payload := range conn.writes {
		require.Equal(t, "gpt-6-astra", payload["model"])
		require.Equal(t, ticket.Identity.SessionID, payload["client_metadata"].(map[string]any)["session_id"])
	}
}

func TestCookieWSSlotReservationBalancesThreeAndWaits(t *testing.T) {
	svc, account, first, _ := newCookieForwardFixture(t, &openAIWSCaptureConn{})
	for slot := 1; slot < openAICookieWSSlotCount; slot++ {
		ticket := *first
		ticket.Slot = slot
		ticket.Generation = fmt.Sprintf("generation-slot-%d", slot)
		ticket.Identity = newOpenAICookieWSIdentity()
		svc.openaiCookieWSTickets.Store(openAICookieWSKeySlot(account.ID, first.Model, slot), &ticket)
	}
	type reservation struct {
		slot    int
		release func()
		err     error
	}
	results := make(chan reservation, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot, release, err := svc.reserveOpenAICookieWSSlot(context.Background(), account, first.Model, "")
			results <- reservation{slot, release, err}
		}()
	}
	wg.Wait()
	close(results)
	counts := [openAICookieWSSlotCount]int{}
	var releases []func()
	for r := range results {
		require.NoError(t, r.err)
		counts[r.slot]++
		releases = append(releases, r.release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	require.Equal(t, [openAICookieWSSlotCount]int{1, 1, 1}, counts)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err := svc.reserveOpenAICookieWSSlot(ctx, account, first.Model, "")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	releases[0]()
	slot, release, err := svc.reserveOpenAICookieWSSlot(context.Background(), account, first.Model, "")
	require.NoError(t, err)
	require.Contains(t, []int{0, 1, 2}, slot)
	release()
}

func TestCookieWSNonAstraModelsUseHTTPResponses(t *testing.T) {
	for _, model := range []string{"gpt-6-sol", "gpt-5.5", "gpt-6-luna"} {
		for _, passthrough := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s_passthrough_%v", model, passthrough), func(t *testing.T) {
				svc, account, _, dialer := newCookieForwardFixture(t, &openAIWSCaptureConn{})
				account.Extra["openai_passthrough"] = passthrough
				account.Extra["openai_oauth_responses_websockets_v2_mode"] = "ctx_pool"
				svc.cfg.Gateway.OpenAIWS.Enabled = true
				svc.cfg.Gateway.OpenAIWS.OAuthEnabled = true
				svc.cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
				upstream := svc.httpUpstream.(*httpUpstreamRecorder)
				upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(cookieWSCompletion(model, "HTTP answer")) + "\n\n"))}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				result, err := svc.Forward(context.Background(), c, account, []byte(fmt.Sprintf(`{"model":%q,"input":"hello","stream":false}`, model)))
				require.NoError(t, err)
				require.NotNil(t, result)
				require.False(t, result.OpenAIWSMode)
				require.Zero(t, dialer.DialCount())
				require.Contains(t, upstream.lastReq.URL.Path, "/responses")
				require.Empty(t, upstream.lastReq.Header.Get("Cookie"))
				require.Contains(t, rec.Body.String(), "HTTP answer")
			})
		}
	}
}

func TestCookieWSNonAstraIngressUsesHTTPBridge(t *testing.T) {
	svc, account, _, dialer := newCookieForwardFixture(t, &openAIWSCaptureConn{})
	upstream := svc.httpUpstream.(*httpUpstreamRecorder)
	upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(cookieWSCompletion("gpt-6-sol", "HTTP bridge")) + "\n\n"))}
	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := coderws.Accept(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		defer ws.CloseNow()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		_, first, err := ws.Read(r.Context())
		if err != nil {
			errCh <- err
			return
		}
		errCh <- svc.ProxyResponsesWebSocketFromClient(r.Context(), c, ws, account, "test-token", first, nil)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-sol","input":"hello"}`)))
	_, response, err := client.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(response), "HTTP bridge")
	_ = client.Close(coderws.StatusNormalClosure, "done")
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("HTTP bridge close timed out")
	}
	require.Zero(t, dialer.DialCount())
	require.Contains(t, upstream.lastReq.URL.Path, "/responses")
}

func TestCookieWSNonAstraAPIKeyForcesResponsesOutsideAllowlist(t *testing.T) {
	svc, account, _, dialer := newCookieForwardFixture(t, &openAIWSCaptureConn{})
	account.ID = 999
	account.Type = AccountTypeAPIKey
	account.Credentials = map[string]any{"api_key": "test-api-key"}
	account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false, "openai_passthrough": false}
	require.True(t, shouldForwardOpenAIResponsesViaRawChatCompletions(account), "fixture has explicit legacy Chat Completions routing")
	upstream := svc.httpUpstream.(*httpUpstreamRecorder)
	upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_http","model":"gpt-6-sol","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"HTTP API key"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-sol","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Zero(t, dialer.DialCount())
	require.Equal(t, "/v1/responses", upstream.lastReq.URL.Path)
	require.Equal(t, "Bearer test-api-key", upstream.lastReq.Header.Get("Authorization"))
	require.Contains(t, rec.Body.String(), "HTTP API key")
}

func TestCookieWSFallbackHelpers(t *testing.T) {
	require.False(t, isOpenAIWSMessageTooBigError(nil))
	require.False(t, isOpenAIWSMessageTooBigError(errors.New("read_event")))
	require.True(t, isOpenAIWSMessageTooBigError(wrapOpenAIWSFallback("message_too_big", errors.New("1009"))))
	require.True(t, isOpenAIWSMessageTooBigError(wrapOpenAIWSFallback("prewarm_message_too_big", errors.New("1009"))))
	require.False(t, shouldOpenAICookieWSHTTPFallback(nil))
	require.True(t, shouldOpenAICookieWSHTTPFallback(wrapOpenAIWSFallback("message_too_big", errors.New("1009"))))
	require.True(t, shouldOpenAICookieWSHTTPFallback(newCookieWSTestError("cookie_ws_validation_not_true", "failed", "Cookie websocket validation probe did not return True; this socket was closed", 0, nil)))
	require.True(t, shouldOpenAICookieWSHTTPFallback(newCookieWSTestError("cookie_ws_validation_closed", "network_error", "closed", 0, nil)))
	require.False(t, shouldOpenAICookieWSHTTPFallback(newCookieWSTestError("cookie_ws_cancelled", "failed", "cancelled", 0, context.Canceled)))
	tooLarge := &openAICookieWSPayloadTooLargeError{PayloadBytes: 300000, ThresholdBytes: 262144}
	require.True(t, shouldOpenAICookieWSHTTPFallback(tooLarge))
	require.Equal(t, openAICookieWSPayloadTooLargeHTTPFallbackReason, cookieWSHTTPFallbackReasonFor(tooLarge))
	require.Equal(t, openAICookieWSPayloadTooLargeHTTPFallbackReason, cookieWSHTTPFallbackReasonFor(wrapOpenAIWSFallback("message_too_big", errors.New("1009"))))
	require.Equal(t, openAICookieWSHTTPFallbackReason, cookieWSHTTPFallbackReasonFor(errOpenAIWSConnQueueFull))
	require.True(t, isOpenAICookieWSHTTPTransportReason(openAICookieWSHTTPFallbackReason))
	require.True(t, isOpenAICookieWSHTTPTransportReason(openAICookieWSPayloadTooLargeHTTPFallbackReason))
	require.False(t, isOpenAICookieWSHTTPTransportReason("cookie_ws"))
	payloadBytes, threshold := cookieWSFallbackPayloadStats(tooLarge)
	require.Equal(t, 300000, payloadBytes)
	require.Equal(t, int64(262144), threshold)
	payloadBytes, threshold = cookieWSFallbackPayloadStats(wrapOpenAIWSFallback("message_too_big", errors.New("1009")))
	require.Equal(t, -1, payloadBytes)
	require.Equal(t, int64(-1), threshold)

	require.Equal(t, openAICookieWSHTTPFallbackThresholdBytesDefault, (*OpenAIGatewayService)(nil).openAICookieWSHTTPFallbackThresholdBytes())
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	require.Zero(t, svc.openAICookieWSHTTPFallbackThresholdBytes())
	require.Nil(t, svc.cookieWSPayloadTooLargeError(1<<30))
	svc.cfg.Gateway.OpenAIWS.CookieWSHTTPFallbackThresholdBytes = openAICookieWSHTTPFallbackThresholdBytesDefault
	require.Nil(t, svc.cookieWSPayloadTooLargeError(100))
	require.NotNil(t, svc.cookieWSPayloadTooLargeError(int(openAICookieWSHTTPFallbackThresholdBytesDefault)))
	svc.cfg.Gateway.OpenAIWS.CookieWSHTTPFallbackThresholdBytes = 1
	require.NotNil(t, svc.cookieWSPayloadTooLargeError(1))
}

func TestCookieWSValidationFailureFallsBackToHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_should_not_use_ws","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"WS"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)}}
	svc, account, _, _ := newCookieForwardFixture(t, conn)
	svc.getOpenAIWSConnPool().SetCookieValidator(func(context.Context, *Account, *openAIWSConnLease) error {
		return newCookieWSTestError("cookie_ws_validation_not_true", "failed", "Cookie websocket validation probe did not return True; this socket was closed", 0, nil)
	})
	upstream := svc.httpUpstream.(*httpUpstreamRecorder)
	upstream.resp = cookieWSHTTPResponse("HTTP after probe")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, openAICookieWSHTTPFallbackReason, c.GetString("openai_ws_transport_reason"))
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
	require.Contains(t, rec.Body.String(), "HTTP after probe")
	require.NotContains(t, rec.Body.String(), "did not return True")
}

func TestCookieWSOversizedPayloadUsesHTTPResponsesWithoutDial(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_should_not_use_ws","model":"gpt-6-astra","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"WS"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`)}}
	svc, account, _, dialer := newCookieForwardFixture(t, conn)
	svc.cfg.Gateway.OpenAIWS.CookieWSHTTPFallbackThresholdBytes = 1
	upstream := svc.httpUpstream.(*httpUpstreamRecorder)
	upstream.resp = cookieWSHTTPResponse("HTTP oversized")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Zero(t, dialer.DialCount(), "oversized Cookie WS must not reserve a slot or dial")
	require.Equal(t, openAICookieWSPayloadTooLargeHTTPFallbackReason, c.GetString("openai_ws_transport_reason"))
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
	require.Empty(t, upstream.lastReq.Header.Get("Cookie"))
	require.Contains(t, rec.Body.String(), "HTTP oversized")
}

func TestCookieWSMessageTooBigFallsBackToHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	conn := &openAIWSCaptureConn{readErr: coderws.CloseError{Code: coderws.StatusMessageTooBig}}
	svc, account, _, dialer := newCookieForwardFixture(t, conn)
	svc.cfg.Gateway.OpenAIWS.CookieWSHTTPFallbackThresholdBytes = 0
	upstream := svc.httpUpstream.(*httpUpstreamRecorder)
	upstream.resp = cookieWSHTTPResponse("HTTP after 1009")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","input":"hello","stream":false}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, 1, dialer.DialCount(), "under-threshold 1009 should try Cookie WS once")
	require.Equal(t, openAICookieWSPayloadTooLargeHTTPFallbackReason, c.GetString("openai_ws_transport_reason"))
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
	require.Contains(t, rec.Body.String(), "HTTP after 1009")
}
