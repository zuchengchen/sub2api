package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSDialForbiddenIsEdge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers http.Header
		body    string
		edge    bool
	}{
		{name: "empty body", body: "", edge: true},
		{name: "cf-mitigated header", headers: http.Header{"Cf-Mitigated": []string{"challenge"}}, body: `{"detail":"x"}`, edge: true},
		{name: "cloudflare challenge html", body: `<!DOCTYPE html><html><head><title>Just a moment...</title></head></html>`, edge: true},
		{name: "cloudflare 1010", body: `error code: 1010`, edge: true},
		{name: "bare text", body: `Forbidden`, edge: true},
		{name: "structured account error", body: `{"error":{"code":"account_deactivated","message":"deactivated"}}`, edge: false},
		{name: "structured detail", body: `{"detail":"Unusual activity has been detected from your device."}`, edge: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dialErr := &openAIWSDialError{StatusCode: http.StatusForbidden, ResponseHeaders: tc.headers, ResponseBody: []byte(tc.body)}
			require.Equal(t, tc.edge, openAIWSDialForbiddenIsEdge(dialErr))
		})
	}
}

func TestOpenAIWSDialForbidden(t *testing.T) {
	_, ok := openAIWSDialForbidden(errors.New("plain"))
	require.False(t, ok)
	_, ok = openAIWSDialForbidden(&openAIWSDialError{StatusCode: http.StatusUnauthorized})
	require.False(t, ok)
	dialErr, ok := openAIWSDialForbidden(wrapOpenAIWSFallback(openAIWSUpstreamForbiddenReason, &openAIWSDialError{StatusCode: http.StatusForbidden}))
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, dialErr.StatusCode)
}

func TestRecordOpenAIWSDialRejectedKeepsEdgeEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	account := &Account{ID: 7, Name: "acc", Platform: PlatformOpenAI}
	err := &openAIWSDialError{
		StatusCode: http.StatusForbidden,
		ResponseHeaders: http.Header{
			"Server":       []string{"cloudflare"},
			"Cf-Ray":       []string{"abc123-LAX"},
			"Cf-Mitigated": []string{"challenge"},
		},
		ResponseBody: []byte(`<html><title>Just a moment...</title></html>`),
		Err:          errors.New("websocket upgrade failed: status 403"),
	}
	recordOpenAIWSDialRejected(c, account, err)

	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events := raw.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	ev := events[0]
	require.Equal(t, "ws_dial_rejected", ev.Kind)
	require.Equal(t, "edge_forbidden", ev.Reason)
	require.Equal(t, http.StatusForbidden, ev.UpstreamStatusCode)
	require.Contains(t, ev.Detail, "server=cloudflare")
	require.Contains(t, ev.Detail, "cf-ray=abc123-LAX")
	require.Contains(t, ev.Detail, "cf-mitigated=challenge")
	require.Contains(t, ev.Detail, "Just a moment")
}

func TestRecordOpenAIWSDialRejectedIgnoresTransportErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	recordOpenAIWSDialRejected(c, &Account{ID: 1}, &openAIWSDialError{Err: errors.New("dial tcp: timeout")})
	_, ok := c.Get(OpsUpstreamErrorsKey)
	require.False(t, ok)
}

// The HTTP Responses path receives a handshake 403 and must transparently
// retry the same request over HTTP instead of returning the 403.
func TestOpenAIGatewayService_Forward_WSv2Dial403FallsBackToHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ws403Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("Cf-Ray", "test-ray")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Just a moment...</title></head></html>`))
	}))
	defer ws403Server.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "custom-client/1.0")

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"usage":{"input_tokens":2,"output_tokens":1,"input_tokens_details":{"cached_tokens":0}}}`,
			)),
		},
	}

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.SessionDisableEnabled = true
	cfg.Gateway.OpenAIWS.SessionDisableRetryTTLMS = 900000

	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     upstream,
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
	}

	account := &Account{
		ID:          91,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": ws403Server.URL,
		},
		Extra: map[string]any{
			"responses_websockets_v2_enabled": true,
		},
	}

	body := []byte(`{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":"hello"}]}`)
	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq, "handshake 403 should fall back to HTTP")
	require.Empty(t, svc.openAIWSSessionDisabledReason(resolveOpenAIWSSessionScope(c, account, body)), "handshake 403 must not pin the session to HTTP")

	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	var dialEvent *OpsUpstreamErrorEvent
	for _, ev := range raw.([]*OpsUpstreamErrorEvent) {
		if ev.Kind == "ws_dial_rejected" {
			dialEvent = ev
		}
	}
	require.NotNil(t, dialEvent, "handshake evidence must be recorded")
	require.Equal(t, "edge_forbidden", dialEvent.Reason)
	require.Contains(t, dialEvent.Detail, "cf-ray=test-ray")
}
