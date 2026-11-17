package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Observe the final transport boundary, after all conversion, identity and
// replay handling. The mock can model the observed encoding-dependent 400
// without sending private history or making a live model request.
type compressionForwardUpstream struct {
	t             *testing.T
	bare400       bool
	rejectEncoded bool
	wire          [][]byte
	decoded       [][]byte
	headers       []http.Header
	tlsProfiles   []*tlsfingerprint.Profile
}

func (u *compressionForwardUpstream) Do(request *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.t.Helper()
	wire, err := io.ReadAll(request.Body)
	require.NoError(u.t, err)
	require.NoError(u.t, request.Body.Close())
	require.Equal(u.t, int64(len(wire)), request.ContentLength, "framing must describe the actual bytes sent")
	require.NotNil(u.t, request.GetBody, "existing authentication recovery needs a replayable body")
	replay, err := request.GetBody()
	require.NoError(u.t, err)
	replayed, err := io.ReadAll(replay)
	require.NoError(u.t, err)
	require.NoError(u.t, replay.Close())
	require.Equal(u.t, wire, replayed, "GetBody must retain the exact final wire bytes")
	decoded := wire
	if request.Header.Get("Content-Encoding") == "zstd" {
		decoder, err := zstd.NewReader(nil)
		require.NoError(u.t, err)
		decoded, err = decoder.DecodeAll(wire, nil)
		decoder.Close()
		require.NoError(u.t, err)
	}
	require.True(u.t, json.Valid(decoded), "the upstream must receive a complete JSON body")
	u.wire = append(u.wire, wire)
	u.decoded = append(u.decoded, decoded)
	u.headers = append(u.headers, request.Header.Clone())
	if u.bare400 || (u.rejectEncoded && request.Header.Get("Content-Encoding") != "") {
		return &http.Response{
			StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"detail":"Bad Request"}`)), Request: request,
		}, nil
	}
	response := `{"id":"resp_compression","object":"response","model":"gpt-6-astra","status":"completed","output":[{"type":"message","id":"msg_compression","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}`
	sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_compression\",\"model\":\"gpt-6-astra\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_compression\",\"output_index\":0,\"content_index\":0,\"delta\":\"OK\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\ndata: [DONE]\n\n"
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse)), Request: request}, nil
}

func (u *compressionForwardUpstream) DoWithTLS(request *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.tlsProfiles = append(u.tlsProfiles, profile)
	return u.Do(request, proxy, accountID, concurrency)
}

const compressionForwardImage = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aP1sAAAAASUVORK5CYII="
const compressionForwardArguments = `{"sequence":9007199254740993,"text":"中文 <tag> &"}`
const compressionForwardOutput = "Synthetic tool result: 中文 <tag> & complete."

func compressionForwardRequest(t *testing.T, route string, stream bool) ([]byte, string) {
	t.Helper()
	// More than one zstd block, with non-ASCII text and an image. Exact block
	// boundary sizes are additionally covered by the transport unit tests.
	text := "Synthetic input 中文 <tag> & " + strings.Repeat("x", 128*1024+1)
	schema := map[string]any{"type": "object", "properties": map[string]any{"sequence": map[string]any{"type": "integer"}, "text": map[string]any{"type": "string"}}, "required": []string{"sequence", "text"}}
	request := map[string]any{"model": "gpt-6-astra", "stream": stream}
	switch route {
	case "chat":
		request["messages"] = []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": compressionForwardImage}}}},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_compression", "type": "function", "function": map[string]any{"name": "fixture_echo", "arguments": compressionForwardArguments}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_compression", "content": compressionForwardOutput},
		}
		request["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "fixture_echo", "parameters": schema}}}
	case "messages":
		request["max_tokens"] = 64
		request["messages"] = []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": strings.TrimPrefix(compressionForwardImage, "data:image/png;base64,")}}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_compression", "name": "fixture_echo", "input": json.RawMessage(compressionForwardArguments)}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_compression", "content": compressionForwardOutput}}},
		}
		request["tools"] = []any{map[string]any{"name": "fixture_echo", "input_schema": schema}}
	default:
		request["instructions"] = "Preserve the complete synthetic input and tool history."
		request["store"] = false
		request["input"] = []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}, map[string]any{"type": "input_image", "image_url": compressionForwardImage}}},
			map[string]any{"type": "function_call", "call_id": "call_compression", "name": "fixture_echo", "arguments": compressionForwardArguments},
			map[string]any{"type": "function_call_output", "call_id": "call_compression", "output": compressionForwardOutput},
		}
		request["tools"] = []any{map[string]any{"type": "function", "name": "fixture_echo", "parameters": schema}}
	}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	return body, text
}

func compressionForwardCall(t *testing.T, route string, stream, compression, bare400, rejectEncoded bool) (*compressionForwardUpstream, *httptest.ResponseRecorder, *OpenAIForwardResult, error, string) {
	t.Helper()
	body, text := compressionForwardRequest(t, route, stream)
	original := append([]byte(nil), body...)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	path := "/v1/responses"
	if route == "chat" {
		path = "/v1/chat/completions"
	} else if route == "messages" {
		path = "/v1/messages"
	}
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "synthetic-compression-regression")
	if route == "lite" {
		c.Request.Header.Set(responsesLiteHeader, "true")
	}
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	cfg := codexSynthEnabledConfig()
	cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
	cfg.Gateway.OpenAIWS.ForceHTTP = true
	cfg.Gateway.OpenAIRequestCompressionEnabled = compression
	upstream := &compressionForwardUpstream{t: t, bare400: bare400, rejectEncoded: rejectEncoded}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, tiboRouteDisabled: true}
	account := newCodexSynthTestAccount(771170, AccountTypeOAuth)
	if route == "passthrough" {
		account.Extra["openai_passthrough"] = true
	}
	var result *OpenAIForwardResult
	var err error
	switch route {
	case "chat":
		result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-6-astra")
	case "messages":
		result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-6-astra")
	default:
		result, err = svc.Forward(context.Background(), c, account, body)
	}
	require.Equal(t, original, body, "the caller's original request must survive forwarding")
	return upstream, recorder, result, err, text
}

func requireCompressionForwardContent(t *testing.T, body []byte, text string) {
	t.Helper()
	var texts, images []string
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("role").String() != "user" {
			continue
		}
		for _, content := range item.Get("content").Array() {
			switch content.Get("type").String() {
			case "input_text":
				texts = append(texts, content.Get("text").String())
			case "input_image":
				images = append(images, content.Get("image_url").String())
			}
		}
	}
	require.Contains(t, texts, text, "the whole large input including Unicode must arrive intact")
	require.Equal(t, []string{compressionForwardImage}, images, "image data must not be truncated or duplicated")
	call := gjson.GetBytes(body, `input.#(type=="function_call")`)
	require.Equal(t, "fixture_echo", call.Get("name").String())
	require.Equal(t, "9007199254740993", gjson.Get(call.Get("arguments").String(), "sequence").Raw)
	require.Equal(t, "中文 <tag> &", gjson.Get(call.Get("arguments").String(), "text").String())
	output := gjson.GetBytes(body, `input.#(type=="function_call_output")`)
	require.Equal(t, call.Get("call_id").String(), output.Get("call_id").String())
	require.Equal(t, compressionForwardOutput, output.Get("output").String())
}

func TestOpenAIHTTPCompressionFullForwardRoutes(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, route := range []string{"responses", "lite", "chat", "messages", "passthrough"} {
		for _, stream := range []bool{false, true} {
			for _, compressed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/compression=%t", route, stream, compressed), func(t *testing.T) {
					upstream, recorder, result, err, text := compressionForwardCall(t, route, stream, compressed, false, false)
					require.NoError(t, err, "downstream=%s", recorder.Body.String())
					require.NotNil(t, result)
					require.Equal(t, http.StatusOK, recorder.Code)
					require.Len(t, upstream.wire, 1)
					require.Len(t, upstream.tlsProfiles, 1, "the compression switch must preserve the Codex TLS path")
					require.NotNil(t, upstream.tlsProfiles[0])
					if compressed {
						require.Equal(t, "zstd", upstream.headers[0].Get("Content-Encoding"))
						require.NotEqual(t, upstream.wire[0], upstream.decoded[0])
					} else {
						require.Empty(t, upstream.headers[0].Get("Content-Encoding"))
						require.Equal(t, upstream.decoded[0], upstream.wire[0])
					}
					requireCompressionForwardContent(t, upstream.decoded[0], text)
				})
			}
		}
	}
}

func TestOpenAIHTTPCompressionEncodingRejectionAndBare400Budget(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, route := range []string{"responses", "lite", "chat", "messages", "passthrough"} {
		for _, test := range []struct {
			name        string
			compression bool
			bare400     bool
			wantError   bool
		}{
			{"raw_avoids_encoding_rejection", false, false, false},
			{"opt_in_reproduces_encoding_rejection", true, false, true},
			{"raw_unrelated_bare_400_is_not_retried", false, true, true},
		} {
			t.Run(route+"/"+test.name, func(t *testing.T) {
				upstream, recorder, _, err, text := compressionForwardCall(t, route, false, test.compression, test.bare400, true)
				if test.wantError {
					require.Error(t, err)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.NotEmpty(t, gjson.GetBytes(recorder.Body.Bytes(), "error.message").String())
				} else {
					require.NoError(t, err)
					require.Equal(t, http.StatusOK, recorder.Code)
				}
				require.Len(t, upstream.wire, 1, "the policy changes encoding, not the retry budget")
				requireCompressionForwardContent(t, upstream.decoded[0], text)
			})
		}
	}
}

func TestOpenAIHTTPCompressionWSSizeFallback(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compression=%t", compressed), func(t *testing.T) {
			var businessFrames atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err == nil {
					businessFrames.Add(1)
				}
			}))
			defer server.Close()
			svc, account, _ := httpBridgeFixture(t, server.URL)
			defer svc.openaiWSPool.Close()
			// Keep the fixture's local dialer: exact fidelity's raw WS dialer
			// otherwise bypasses it. The forwarding and HTTP transport config
			// still exercise exact fidelity and compression independently.
			poolConfig := *svc.cfg
			svc.openaiWSPool.cfg = &poolConfig
			svc.cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
			svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
			svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 64 * 1024
			svc.cfg.Gateway.OpenAIRequestCompressionEnabled = compressed
			upstream := &compressionForwardUpstream{t: t}
			svc.httpUpstream = upstream
			body, text := compressionForwardRequest(t, "responses", false)
			c, _ := httpBridgeContext("compression-size-fallback")
			result, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.OpenAIWSMode)
			require.Equal(t, "payload_too_large", c.GetString("openai_ws_session_fallback_reason"))
			require.Zero(t, businessFrames.Load(), "size preflight must prevent an oversized WS send")
			require.Len(t, upstream.wire, 1)
			if compressed {
				require.Equal(t, "zstd", upstream.headers[0].Get("Content-Encoding"))
			} else {
				require.Empty(t, upstream.headers[0].Get("Content-Encoding"))
			}
			requireCompressionForwardContent(t, upstream.decoded[0], text)
		})
	}
}

func TestOpenAIHTTPCompressionWSIngressHTTPBridge(t *testing.T) {
	pinCodexTestVersion(t, "0.166.0")
	for _, compressed := range []bool{false, true} {
		for _, bare400 := range []bool{false, true} {
			t.Run(fmt.Sprintf("compression=%t/bare400=%t", compressed, bare400), func(t *testing.T) {
				cfg := codexSynthEnabledConfig()
				cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
				cfg.Gateway.OpenAIRequestCompressionEnabled = compressed
				upstream := &compressionForwardUpstream{t: t, bare400: bare400}
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, tiboRouteDisabled: true}
				account := newCodexSynthTestAccount(771171, AccountTypeOAuth)
				body, text := compressionForwardRequest(t, "responses", true)
				payload := append([]byte(`{"type":"response.create",`), body[1:]...)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
				var events [][]byte
				result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
					context.Background(), c, account, "synthetic-token", payload, len(payload),
					"gpt-6-astra", "", "", "", "", 1,
					func(event []byte) error {
						events = append(events, bytes.Clone(event))
						return nil
					},
				)
				if bare400 {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Contains(t, string(bytes.Join(events, nil)), "response.completed")
				}
				require.Len(t, upstream.wire, 1, "a generic upstream 400 must not create an extra bridge attempt")
				if compressed {
					require.Equal(t, "zstd", upstream.headers[0].Get("Content-Encoding"))
				} else {
					require.Empty(t, upstream.headers[0].Get("Content-Encoding"))
				}
				requireCompressionForwardContent(t, upstream.decoded[0], text)
			})
		}
	}
}
