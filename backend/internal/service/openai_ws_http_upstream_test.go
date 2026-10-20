//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newOpenAIWSHTTPUpstreamTestConfig(modeRouter bool) *config.Config {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = modeRouter
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	return cfg
}

type openAIWSHTTPUpstreamHarness struct {
	svc      *OpenAIGatewayService
	dialer   *openAIWSCaptureDialer
	conn     *openAIWSCaptureConn
	upstream *httpUpstreamRecorder
}

func newOpenAIWSHTTPUpstreamHarness(cfg *config.Config, events ...string) *openAIWSHTTPUpstreamHarness {
	conn := &openAIWSCaptureConn{}
	for _, event := range events {
		conn.events = append(conn.events, []byte(event))
	}
	dialer := &openAIWSCaptureDialer{conn: conn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:               cfg,
		httpUpstream:      upstream,
		cache:             &stubGatewayCache{},
		openaiWSResolver:  NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:     NewCodexToolCorrector(),
		openaiWSPool:      pool,
		tiboRouteDisabled: true,
	}
	return &openAIWSHTTPUpstreamHarness{svc: svc, dialer: dialer, conn: conn, upstream: upstream}
}

func newOpenAIWSHTTPUpstreamContext(path string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c, rec
}

func newOpenAIWSHTTPUpstreamOAuthAccount(extra map[string]any) *Account {
	return &Account{
		ID:          8801,
		Name:        "openai-oauth-ws-default",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-acc"},
		Extra:       extra,
	}
}

const openAIWSHTTPUpstreamCompleted = `{"type":"response.completed","response":{"id":"resp_ws_http_1","model":"gpt-5.5","usage":{"input_tokens":2,"output_tokens":1}}}`

func TestOpenAIWSHTTPUpstream_OAuthHTTPClientDecisionMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","stream":true,"instructions":"test","input":[{"type":"input_text","text":"hi"}]}`)

	cases := []struct {
		name       string
		modeRouter bool
		extra      map[string]any
		wantWS     bool
		wantReason string
	}{
		{name: "unset_default_router", modeRouter: false, extra: nil, wantWS: true, wantReason: "ws_v2_enabled"},
		{name: "unset_mode_router", modeRouter: true, extra: nil, wantWS: true, wantReason: "ws_v2_mode_ctx_pool"},
		{name: "explicit_ctx_pool", modeRouter: true, extra: map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool}, wantWS: true, wantReason: "ws_v2_mode_ctx_pool"},
		{name: "explicit_mode_off_router", modeRouter: true, extra: map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff}, wantWS: false, wantReason: "account_mode_off"},
		{name: "explicit_mode_off_default_router", modeRouter: false, extra: map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff}, wantWS: false, wantReason: "account_disabled"},
		{name: "explicit_enabled_false", modeRouter: false, extra: map[string]any{"openai_oauth_responses_websockets_v2_enabled": false}, wantWS: false, wantReason: "account_disabled"},
		{name: "account_force_http", modeRouter: true, extra: map[string]any{"openai_ws_force_http": true}, wantWS: false, wantReason: "account_force_http"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newOpenAIWSHTTPUpstreamHarness(newOpenAIWSHTTPUpstreamTestConfig(tc.modeRouter), openAIWSHTTPUpstreamCompleted)
			c, rec := newOpenAIWSHTTPUpstreamContext("/openai/v1/responses")
			result, err := h.svc.Forward(context.Background(), c, newOpenAIWSHTTPUpstreamOAuthAccount(tc.extra), body)
			require.NoError(t, err)
			require.NotNil(t, result)
			reason, _ := c.Get("openai_ws_transport_reason")
			require.Equal(t, tc.wantReason, reason)
			require.Equal(t, tc.wantWS, result.OpenAIWSMode)
			if tc.wantWS {
				require.Equal(t, 1, h.dialer.DialCount(), "OAuth HTTP client must dial the WS v2 upstream")
				require.Nil(t, h.upstream.lastReq, "WS turn must not also hit the HTTP upstream")
				require.Contains(t, rec.Body.String(), "response.completed")
				require.Equal(t, "response.create", gjson.Get(requestToJSONString(h.conn.lastWrite), "type").String())
			} else {
				require.Equal(t, 0, h.dialer.DialCount())
				require.NotNil(t, h.upstream.lastReq, "opted-out account must stay on HTTP")
			}
		})
	}
}

func TestOpenAIWSHTTPUpstream_GlobalForceHTTPKeepsOAuthOnHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSHTTPUpstreamTestConfig(true)
	cfg.Gateway.OpenAIWS.ForceHTTP = true
	h := newOpenAIWSHTTPUpstreamHarness(cfg, openAIWSHTTPUpstreamCompleted)
	c, _ := newOpenAIWSHTTPUpstreamContext("/openai/v1/responses")
	result, err := h.svc.Forward(context.Background(), c, newOpenAIWSHTTPUpstreamOAuthAccount(nil), []byte(`{"model":"gpt-5.5","stream":true,"instructions":"test","input":[{"type":"input_text","text":"hi"}]}`))
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	reason, _ := c.Get("openai_ws_transport_reason")
	require.Equal(t, "global_force_http", reason)
	require.Equal(t, 0, h.dialer.DialCount())
}

func TestOpenAIWSHTTPUpstream_CompactStaysHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newOpenAIWSHTTPUpstreamHarness(newOpenAIWSHTTPUpstreamTestConfig(true), openAIWSHTTPUpstreamCompleted)
	h.upstream.resp = &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"cmp_1","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}
	c, _ := newOpenAIWSHTTPUpstreamContext("/openai/v1/responses/compact")
	_, _ = h.svc.Forward(context.Background(), c, newOpenAIWSHTTPUpstreamOAuthAccount(nil), []byte(`{"model":"gpt-5.5","instructions":"test","input":[{"type":"input_text","text":"hi"}]}`))
	reason, _ := c.Get("openai_ws_transport_reason")
	require.Equal(t, "compact_http", reason)
	require.Equal(t, 0, h.dialer.DialCount(), "compact must never dial the WS upstream")
}

func TestOpenAIWSHTTPUpstream_APIKeyHTTPClientPreservesHTTPRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := newOpenAIWSHTTPUpstreamHarness(newOpenAIWSHTTPUpstreamTestConfig(true), openAIWSHTTPUpstreamCompleted)
	wsFallback := httptest.NewServer(http.NotFoundHandler())
	defer wsFallback.Close()
	c, _ := newOpenAIWSHTTPUpstreamContext("/openai/v1/responses")
	account := &Account{
		ID: 8802, Name: "openai-apikey", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": wsFallback.URL},
		Extra:       map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool},
	}
	result, err := h.svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":[{"type":"input_text","text":"hi"}]}`))
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	reason, _ := c.Get("openai_ws_transport_reason")
	require.Equal(t, "http_relay_preserved", reason)
	require.Equal(t, 0, h.dialer.DialCount())
}

type openAIWSGracefulCloseConn struct {
	*openAIWSCaptureConn
	closeErr error
}

func (c *openAIWSGracefulCloseConn) ReadMessage(ctx context.Context) ([]byte, error) {
	message, err := c.openAIWSCaptureConn.ReadMessage(ctx)
	if err == io.EOF {
		return nil, c.closeErr
	}
	return message, err
}

type openAIWSGracefulCloseDialer struct {
	conn *openAIWSGracefulCloseConn
}

func (d *openAIWSGracefulCloseDialer) Dial(context.Context, string, http.Header, string) (openAIWSClientConn, int, http.Header, error) {
	return d.conn, 0, http.Header{}, nil
}

func TestOpenAIWSHTTPUpstream_GracefulCloseAfterOutputEmitsIncomplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSHTTPUpstreamTestConfig(true)
	h := newOpenAIWSHTTPUpstreamHarness(cfg)
	conn := &openAIWSGracefulCloseConn{
		openAIWSCaptureConn: &openAIWSCaptureConn{events: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_graceful","model":"gpt-5.5"}}`),
			[]byte(`{"type":"response.output_text.delta","response_id":"resp_graceful","delta":"partial"}`),
		}},
		closeErr: coderws.CloseError{Code: coderws.StatusGoingAway, Reason: "server restart"},
	}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSGracefulCloseDialer{conn: conn})
	h.svc.openaiWSPool = pool

	c, rec := newOpenAIWSHTTPUpstreamContext("/openai/v1/responses")
	result, err := h.svc.Forward(context.Background(), c, newOpenAIWSHTTPUpstreamOAuthAccount(nil), []byte(`{"model":"gpt-5.5","stream":true,"instructions":"test","input":[{"type":"input_text","text":"hi"}]}`))
	require.NoError(t, err, "a graceful upstream close after output must end the stream, not fail the turn")
	require.NotNil(t, result)
	require.Equal(t, "response.incomplete", result.UpstreamTerminalEvent)
	out := rec.Body.String()
	require.Contains(t, out, "partial")
	require.Contains(t, out, `"type":"response.incomplete"`)
	require.Contains(t, out, `"reason":"server_error"`)
	require.Nil(t, h.upstream.lastReq)
}

func TestIsOpenAIWSGracefulClose(t *testing.T) {
	require.True(t, isOpenAIWSGracefulClose(coderws.CloseError{Code: coderws.StatusNormalClosure}))
	require.True(t, isOpenAIWSGracefulClose(coderws.CloseError{Code: coderws.StatusGoingAway}))
	require.True(t, isOpenAIWSGracefulClose(coderws.CloseError{Code: coderws.StatusServiceRestart}))
	require.False(t, isOpenAIWSGracefulClose(coderws.CloseError{Code: coderws.StatusInternalError}))
	require.False(t, isOpenAIWSGracefulClose(io.EOF))
	require.False(t, isOpenAIWSGracefulClose(nil))
}

func TestOpenAIWSErrorEventPolicySignal(t *testing.T) {
	status, body := openAIWSErrorEventPolicySignal([]byte(`{"type":"error","error":{"type":"server_err","code":"server_err","message":"boom"}}`))
	require.Equal(t, http.StatusBadGateway, status)
	require.Equal(t, "server_error", gjson.GetBytes(body, "error.code").String())
	require.Equal(t, "upstream_error", gjson.GetBytes(body, "error.type").String())
	require.Equal(t, "boom", gjson.GetBytes(body, "error.upstream_message").String())

	status, _ = openAIWSErrorEventPolicySignal([]byte(`{"type":"error","error":{"code":"server_is_overloaded","message":"busy"}}`))
	require.Equal(t, http.StatusServiceUnavailable, status)

	status, body = openAIWSErrorEventPolicySignal([]byte(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"bad"}}`))
	require.Equal(t, 0, status)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(body, "error.type").String())

	reason, retryable := classifyOpenAIWSErrorEventFromRaw("server_err", "server_err", "boom")
	require.Equal(t, "upstream_error_event", reason)
	require.True(t, retryable)
}

type openAIWSProxyRecordingDialer struct {
	*openAIWSCaptureDialer
	proxyURLs []string
}

func (d *openAIWSProxyRecordingDialer) Dial(ctx context.Context, wsURL string, headers http.Header, proxyURL string) (openAIWSClientConn, int, http.Header, error) {
	d.proxyURLs = append(d.proxyURLs, proxyURL)
	return d.openAIWSCaptureDialer.Dial(ctx, wsURL, headers, proxyURL)
}

func TestOpenAIWSHTTPUpstream_WSDialUsesAccountProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSHTTPUpstreamTestConfig(true)
	h := newOpenAIWSHTTPUpstreamHarness(cfg, openAIWSHTTPUpstreamCompleted)
	recording := &openAIWSProxyRecordingDialer{openAIWSCaptureDialer: h.dialer}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(recording)
	h.svc.openaiWSPool = pool

	proxyID := int64(77)
	account := newOpenAIWSHTTPUpstreamOAuthAccount(nil)
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 18080}

	c, _ := newOpenAIWSHTTPUpstreamContext("/openai/v1/responses")
	result, err := h.svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.5","stream":true,"instructions":"test","input":[{"type":"input_text","text":"hi"}]}`))
	require.NoError(t, err)
	require.True(t, result.OpenAIWSMode)
	require.Equal(t, []string{"http://127.0.0.1:18080"}, recording.proxyURLs, "the WS upstream dial must use the account proxy, never a direct connection")
}

func TestOpenAIWSIngress_UpstreamDialUsesAccountProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSHTTPUpstreamTestConfig(true)
	h := newOpenAIWSHTTPUpstreamHarness(cfg, openAIWSHTTPUpstreamCompleted)
	recording := &openAIWSProxyRecordingDialer{openAIWSCaptureDialer: h.dialer}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(recording)
	h.svc.openaiWSPool = pool

	proxyID := int64(78)
	account := newOpenAIWSHTTPUpstreamOAuthAccount(map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool})
	account.ProxyID = &proxyID
	account.Proxy = &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 18081}

	serverErrCh := make(chan error, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		defer func() { _ = conn.CloseNow() }()
		rec := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(rec)
		ginCtx.Request = r.Clone(r.Context())
		ginCtx.Request.Header.Set("User-Agent", "unit-test-agent/1.0")
		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		_, first, readErr := conn.Read(readCtx)
		cancel()
		if readErr != nil {
			serverErrCh <- readErr
			return
		}
		serverErrCh <- h.svc.ProxyResponsesWebSocketFromClient(r.Context(), ginCtx, conn, account, "oauth-token", first, nil)
	}))
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http"), nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	require.NoError(t, clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.5","stream":true}`)))
	cancelWrite()
	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, event, readErr := clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, readErr)
	require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
	require.NoError(t, clientConn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case <-serverErrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("ingress websocket did not finish")
	}
	require.NotEmpty(t, recording.proxyURLs)
	for _, got := range recording.proxyURLs {
		require.Equal(t, "http://127.0.0.1:18081", got, "ingress upstream dial must use the account proxy")
	}
}
