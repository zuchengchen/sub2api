package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const codexHTTPLiteInput = `[{"type":"additional_tools","id":"at_test","role":"developer","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},{"type":"message","id":"msg_test","role":"user","content":[{"type":"input_text","text":"keep Unicode 中文 and ws_request_header_x_openai_internal_codex_responses_lite"}],"ordinal":9007199254740993}]`

func codexHTTPLiteBody(withMarker bool) []byte {
	metadata := `"opaque":"keep"`
	if withMarker {
		metadata += `,"ws_request_header_x_openai_internal_codex_responses_lite":"true"`
	}
	return []byte(`{"model":"gpt-6-astra","stream":true,"input":` + codexHTTPLiteInput + `,"tools":[],"parallel_tool_calls":false,"reasoning":{"effort":"xhigh","context":"all_turns"},"client_metadata":{` + metadata + `}}`)
}

func TestCodexHTTPMetadataMovesOnlyWSLiteDeclarationToHeader(t *testing.T) {
	for _, header := range []string{"", "true", "false"} {
		t.Run("header_"+header, func(t *testing.T) {
			original := codexHTTPLiteBody(true)
			req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(original))
			if header != "" {
				req.Header.Set(responsesLiteHeader, header)
			}
			account := liteTestAccount(nil)
			next, err := applyCodexHTTPWireMetadata(req, original, account)
			require.NoError(t, err)
			require.False(t, gjson.GetBytes(next, "client_metadata."+responsesLiteWSMetadataKey).Exists(), "HTTP carries this capability in a header, not the WS metadata namespace")
			want := header
			if want == "" {
				want = "true"
			}
			require.Equal(t, want, req.Header.Get(responsesLiteHeader), "an explicit HTTP declaration keeps precedence")
			require.Equal(t, codexHTTPLiteInput, gjson.GetBytes(next, "input").Raw)
			require.Equal(t, gjson.GetBytes(original, "tools").Raw, gjson.GetBytes(next, "tools").Raw)
			require.Equal(t, gjson.GetBytes(original, "reasoning").Raw, gjson.GetBytes(next, "reasoning").Raw)
			require.Equal(t, "gpt-6-astra", gjson.GetBytes(next, "model").String())
			require.Equal(t, "keep", gjson.GetBytes(next, "client_metadata.opaque").String())
			require.True(t, isOpenAIResponsesLiteWebSocketPayload(original), "the immutable ingress frame remains usable for retries")
			actual, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, next, actual)
			replay, err := req.GetBody()
			require.NoError(t, err)
			defer replay.Close()
			replayBody, err := io.ReadAll(replay)
			require.NoError(t, err)
			require.Equal(t, next, replayBody)
			require.Equal(t, int64(len(next)), req.ContentLength)
		})
	}
}

func TestCodexHTTPMetadataDoesNotInventLiteFromAllTurns(t *testing.T) {
	original := codexHTTPLiteBody(false)
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(original))
	next, err := applyCodexHTTPWireMetadata(req, original, liteTestAccount(nil))
	require.NoError(t, err)
	require.Empty(t, req.Header.Get(responsesLiteHeader))
	require.False(t, isOpenAIResponsesLiteWebSocketPayload(next))
	require.Equal(t, codexHTTPLiteInput, gjson.GetBytes(next, "input").Raw)
}

func TestCodexHTTPMetadataLeavesAPIKeyAndNativeWSProjectionUnchanged(t *testing.T) {
	original := codexHTTPLiteBody(true)
	req := httptest.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(original))
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	next, err := applyCodexHTTPWireMetadata(req, original, account)
	require.NoError(t, err)
	require.Equal(t, original, next)
	require.Empty(t, req.Header.Get(responsesLiteHeader))
	var frame map[string]any
	require.NoError(t, decodeOpenAIJSONUseNumber(original, &frame))
	applyCodexWireLaneMetadata(frame, nil, liteTestAccount(nil), "turn")
	require.True(t, liteMarkerValue(t, frame))
	raw, err := json.Marshal(frame)
	require.NoError(t, err)
	require.Equal(t, "xhigh", gjson.GetBytes(raw, "reasoning.effort").String())
	require.Equal(t, "all_turns", gjson.GetBytes(raw, "reasoning.context").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(raw, "input.1.ordinal").Raw)
}

func TestCodexHTTPBuildersProjectLiteWithoutChangingInput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "regular"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("User-Agent", "codex_exec/0.166.0")
			c.Request.Header.Set(responsesLiteHeader, "true")
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := liteTestAccount(nil)
			body := codexHTTPLiteBody(true)
			var req *http.Request
			var err error
			if passthrough {
				req, err = svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "synthetic")
			} else {
				req, err = svc.buildUpstreamRequest(context.Background(), c, account, body, "synthetic", true, "", true)
			}
			require.NoError(t, err)
			defer req.Body.Close()
			actual, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, "true", req.Header.Get(responsesLiteHeader))
			require.False(t, isOpenAIResponsesLiteWebSocketPayload(actual))
			require.Equal(t, codexHTTPLiteInput, gjson.GetBytes(actual, "input").Raw)
			require.Equal(t, "xhigh", gjson.GetBytes(actual, "reasoning.effort").String())
			require.Equal(t, "gpt-6-astra", gjson.GetBytes(actual, "model").String())
		})
	}
}

func TestCodexWSHTTPBridgeProjectsLiteDeclarationForActualHTTPRequest(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(autoContinuationEvent("resp_lite", autoContinuationAnswer)) + "\n\n"))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream}
	account := liteTestAccount(astraHealthyTestExtra(nil))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	// Native clients declare Lite per frame. No upgrade/HTTP Lite header is needed.
	payload := codexHTTPLiteBody(true)
	var frames [][]byte
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "synthetic", payload, len(payload), "gpt-6-astra", "", "", "", "", 1, func(frame []byte) error { frames = append(frames, append([]byte(nil), frame...)); return nil })
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotEmpty(t, frames)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "true", upstream.lastReq.Header.Get(responsesLiteHeader))
	require.False(t, isOpenAIResponsesLiteWebSocketPayload(upstream.lastBody))
	require.Equal(t, "gpt-6-astra", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "xhigh", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
	require.Equal(t, "all_turns", gjson.GetBytes(upstream.lastBody, "reasoning.context").String())
	require.JSONEq(t, codexHTTPLiteInput, gjson.GetBytes(upstream.lastBody, "input").Raw)
	require.Equal(t, "9007199254740993", gjson.GetBytes(upstream.lastBody, "input.1.ordinal").Raw)
	require.True(t, isOpenAIResponsesLiteWebSocketPayload(payload), "bridge cannot mutate the native input frame")
}
