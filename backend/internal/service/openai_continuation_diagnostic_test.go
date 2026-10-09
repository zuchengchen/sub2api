package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const diagnosticContinuationOutput = `[{"type":"reasoning","id":"rs_diagnostic","summary":[],"encrypted_content":"fixture-encrypted-content"},{"type":"message","id":"msg_diagnostic","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Chosen invariant: preserve every prior decision."}]},{"type":"function_call","id":"fc_diagnostic","call_id":"call_diagnostic","name":"inspect","arguments":"{}"}]`

func diagnosticContinuationSSE(id, output string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":{"id":"` + id + `","model":"gpt-6-astra","status":"completed","output":` + output + `,"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n")),
	}
}

func diagnosticContinuationConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	return cfg
}

func diagnosticContinuationAccount() *Account {
	return &Account{
		ID: 7799, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"access_token": "fixture-token", "chatgpt_account_id": "fixture-account"},
		Extra:       map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeHTTPBridge},
	}
}

func TestDiagnosticOAuthHTTPExplicitHistoryPreservesAssistantAndReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := diagnosticContinuationConfig()
	upstream := &httpUpstreamRecorder{resp: diagnosticContinuationSSE("resp_http", `[]`)}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), tiboRouteDisabled: true}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	body := []byte(`{"model":"gpt-6-astra","instructions":"Follow the supplied request.","stream":true,"store":false,"reasoning":{"effort":"xhigh"},"input":[{"role":"user","content":"Inspect the fixture."},` + strings.TrimSuffix(strings.TrimPrefix(diagnosticContinuationOutput, "["), "]") + `,{"type":"function_call_output","call_id":"call_diagnostic","output":"fixture result"}]}`)
	result, err := svc.Forward(context.Background(), c, diagnosticContinuationAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, string(upstream.lastBody), "fixture-encrypted-content")
	require.Contains(t, string(upstream.lastBody), "Chosen invariant: preserve every prior decision.")
	require.Equal(t, "xhigh", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
	decision, _ := c.Get("openai_ws_transport_decision")
	reason, _ := c.Get("openai_ws_transport_reason")
	require.Equal(t, string(OpenAIUpstreamTransportHTTPSSE), decision)
	require.Equal(t, "client_protocol_http", reason)
}

func TestDiagnosticOAuthWSHTTPBridgeIncrementalHistoryPreservesAssistantAndReasoning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := diagnosticContinuationConfig()
	output := strings.TrimSuffix(diagnosticContinuationOutput, "]") + `,{"type":"reasoning","id":"rs_id_only","summary":[]}]`
	first := diagnosticContinuationSSE("resp_first", output)
	var outputEvents strings.Builder
	for _, item := range gjson.Parse(output).Array() {
		outputEvents.WriteString(`data: {"type":"response.output_item.done","item":` + item.Raw + "}\n\n")
	}
	first.Body = io.NopCloser(io.MultiReader(strings.NewReader(outputEvents.String()), first.Body))
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		first,
		diagnosticContinuationSSE("resp_second", `[]`),
	}}
	svc := &OpenAIGatewayService{
		cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(),
		tiboRouteDisabled: true,
	}
	errCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		defer conn.CloseNow()
		readCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		_, first, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			errCh <- err
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.Clone(r.Context())
		errCh <- svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, diagnosticContinuationAccount(), "fixture-token", first, nil)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	for _, payload := range []string{
		`{"type":"response.create","model":"gpt-6-astra","instructions":"Follow the supplied request.","reasoning":{"effort":"xhigh"},"input":[{"role":"user","content":"Inspect the fixture."}]}`,
		`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_first","input":[{"type":"function_call_output","call_id":"call_diagnostic","output":"fixture result"}]}`,
	} {
		require.NoError(t, client.Write(ctx, websocket.MessageText, []byte(payload)))
		for {
			_, event, readErr := client.Read(ctx)
			require.NoError(t, readErr)
			if gjson.GetBytes(event, "type").String() == "response.completed" {
				break
			}
		}
	}
	require.NoError(t, client.Close(websocket.StatusNormalClosure, "done"))
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Len(t, upstream.bodies, 2)
	second := upstream.bodies[1]
	t.Logf("second HTTP request input: %s", gjson.GetBytes(second, "input").Raw)
	require.False(t, gjson.GetBytes(second, "previous_response_id").Exists())
	require.Contains(t, string(second), "fixture-encrypted-content", "encrypted reasoning must survive stateless continuation")
	require.Contains(t, string(second), "Chosen invariant: preserve every prior decision.", "assistant decisions must survive stateless continuation")
	items := gjson.GetBytes(second, "input").Array()
	require.Len(t, items, 5, "output_item.done and response.completed must be deduplicated")
	require.Equal(t, "user", items[0].Get("role").String())
	require.Equal(t, "reasoning", items[1].Get("type").String())
	require.Equal(t, "message", items[2].Get("type").String())
	require.Equal(t, "commentary", items[2].Get("phase").String())
	require.Equal(t, "function_call", items[3].Get("type").String())
	require.Equal(t, "function_call_output", items[4].Get("type").String())
	require.NotContains(t, string(second), "rs_id_only")
}

func TestDiagnosticBridgeReplayFilterPreservesFailoverItems(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_id_only","summary":[]}`),
		json.RawMessage(`{"type":"reasoning","id":"rs_encrypted","encrypted_content":"fixture-ciphertext"}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[]}`),
		json.RawMessage(`{"type":"function_call","call_id":"fixture-call","name":"inspect","arguments":"{}"}`),
	}
	replayable := openAIWSHTTPBridgeReplayOutputItems(items)
	require.Equal(t, items[1:], replayable)
	require.Len(t, items, 4, "filter must not mutate the full failover history")
	require.Equal(t, "rs_id_only", gjson.GetBytes(items[0], "id").String())
}
