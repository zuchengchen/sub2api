package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Only the network destination is replaced: the production OAuth resolver,
// forwarder, WS handshake and response conversion all execute normally.
type httpBridgeLocalDialer struct{ url string }

func (d *httpBridgeLocalDialer) Dial(ctx context.Context, _ string, h http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	return newDefaultOpenAIWSClientDialer().Dial(ctx, d.url, h, proxy)
}

func httpBridgeFixture(t *testing.T, upstreamURL string) (*OpenAIGatewayService, *Account, *httpUpstreamSequenceRecorder) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
	cfg.Gateway.OpenAIWS.DeltaContextEnabled = true
	cfg.Gateway.OpenAIWS.SessionDisableEnabled = true
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 2
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&httpBridgeLocalDialer{url: "ws" + strings.TrimPrefix(upstreamURL, "http")})
	t.Cleanup(pool.Close)
	u := &httpUpstreamSequenceRecorder{}
	for i := 0; i < 5; i++ {
		u.responses = append(u.responses, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_http","usage":{"input_tokens":8,"output_tokens":2}}`))})
	}
	s := &OpenAIGatewayService{tiboRouteDisabled: true, cfg: cfg, httpUpstream: u, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSPool: pool, toolCorrector: NewCodexToolCorrector()}
	a := &Account{Extra: astraHealthyTestExtra(map[string]any{"responses_websockets_v2_enabled": true}), ID: 23, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 2, Credentials: map[string]any{"access_token": "fake-oauth-token"}}
	return s, a, u
}

func httpBridgeContext(session string) (*gin.Context, *httptest.ResponseRecorder) {
	r := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("session-id", session)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	c.Set("api_key", &APIKey{ID: 123})
	return c, r
}

const autoContinuationBase = `{"type":"message","id":"msg_base","role":"developer","content":[{"type":"input_text","text":"Preserve this context and use lookup."}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}}`
const autoContinuationUser = `{"type":"message","role":"user","content":[{"type":"input_text","text":"Look it up."}]}`
const autoContinuationCall = `{"type":"function_call","id":"fc_test","call_id":"call_lookup","name":"lookup","arguments":"{\"key\":\"test\"}","status":"completed"}`
const autoContinuationTool = `{"type":"function_call_output","call_id":"call_lookup","output":"lookup result"}`
const autoContinuationAnswer = `{"type":"message","id":"msg_answer","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done","annotations":[]}]}`

func autoContinuationBody(stream bool, items ...string) []byte {
	input := []json.RawMessage{json.RawMessage(autoContinuationBase), json.RawMessage(autoContinuationUser)}
	for _, item := range items {
		input = append(input, json.RawMessage(item))
	}
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-6-astra", "stream": stream, "store": false,
		"reasoning": map[string]any{"effort": "xhigh"}, "input": input,
		"include": []string{"reasoning.encrypted_content"},
		"tools":   []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}}}},
	})
	return body
}

func autoContinuationPayload(t *testing.T, items ...string) map[string]any {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal(autoContinuationBody(true, items...), &payload))
	payload["type"] = "response.create"
	return payload
}

func autoContinuationEvent(id, output string) []byte {
	return []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","model":"gpt-6-astra","output":[%s],"usage":{"input_tokens":100,"output_tokens":5,"input_tokens_details":{"cached_tokens":90}}}}`, id, output))
}

func TestHTTPToWSAutoContinuationToolAndText(t *testing.T) {
	for _, test := range []struct{ stream, custom, sparse bool }{{false, false, false}, {true, false, false}, {false, true, false}, {true, true, false}, {true, false, true}, {true, true, true}} {
		t.Run(fmt.Sprintf("stream_%t/custom_%t/sparse_%t", test.stream, test.custom, test.sparse), func(t *testing.T) {
			toolCall, toolResult := autoContinuationCall, autoContinuationTool
			if test.custom {
				toolCall = `{"type":"custom_tool_call","id":"ctc_test","call_id":"call_lookup","name":"exec","input":"lookup()","status":"completed"}`
				toolResult = `{"type":"custom_tool_call_output","id":"ctco_test","call_id":"call_lookup","output":[{"type":"input_text","text":"lookup result"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn_two","create_time":123.0}}`
			}
			makeBody := func(items ...string) []byte {
				body := autoContinuationBody(test.stream, items...)
				if test.custom {
					var request map[string]any
					require.NoError(t, json.Unmarshal(body, &request))
					request["tools"] = []any{map[string]any{"type": "custom", "name": "exec", "format": map[string]any{"type": "text"}}}
					body, _ = json.Marshal(request)
				}
				return body
			}
			requests := make(chan []byte, 8)
			var dials atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				dials.Add(1)
				for turn := 1; ; turn++ {
					_, raw, err := conn.ReadMessage()
					if err != nil {
						return
					}
					requests <- raw
					output := autoContinuationAnswer
					if turn == 1 {
						output = toolCall
					}
					if test.sparse {
						if conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s}`, output))) != nil {
							return
						}
						output = ""
					}
					if conn.WriteMessage(websocket.TextMessage, autoContinuationEvent(fmt.Sprintf("resp_%d", turn), output)) != nil {
						return
					}
				}
			}))
			defer server.Close()
			s, a, httpUpstream := httpBridgeFixture(t, server.URL)
			defer s.openaiWSPool.Close()
			turns := [][]byte{
				makeBody(),
				makeBody(toolCall, toolResult),
				makeBody(toolCall, toolResult, autoContinuationAnswer, `{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}`),
			}
			for i, body := range turns {
				c, _ := httpBridgeContext("auto-incremental")
				result, err := s.Forward(context.Background(), c, a, body)
				require.NoError(t, err)
				require.True(t, result.OpenAIWSMode)
				require.Equal(t, i > 0, c.GetBool("openai_ws_auto_incremental_attempted"))
				require.Equal(t, i > 0, c.GetBool("openai_ws_auto_incremental"), c.GetString("openai_ws_auto_incremental_reason"))
				require.Equal(t, i > 0, c.GetBool("openai_ws_auto_incremental_succeeded"))
				raw := <-requests
				require.Equal(t, "gpt-6-astra", gjson.GetBytes(raw, "model").String())
				require.Equal(t, "xhigh", gjson.GetBytes(raw, "reasoning.effort").String())
				if i == 0 {
					require.False(t, gjson.GetBytes(raw, "previous_response_id").Exists())
				} else {
					require.Equal(t, fmt.Sprintf("resp_%d", i), gjson.GetBytes(raw, "previous_response_id").String())
					require.Len(t, gjson.GetBytes(raw, "input").Array(), 1)
					if i == 1 {
						require.Equal(t, "call_lookup", gjson.GetBytes(raw, "input.0.call_id").String())
					}
				}
			}
			require.Equal(t, int32(1), dials.Load(), "all delta turns must use the original upstream socket")
			require.Zero(t, httpUpstream.callCount)
		})
	}
}

func TestHTTPToWSAutoContinuationRecoveryKeepsFullTools(t *testing.T) {
	for _, mode := range []string{"anchor_miss", "failed_anchor_miss", "http_fallback", "read_retry", "missing_tool", "failed_missing_tool"} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan []byte, 8)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					_, raw, err := conn.ReadMessage()
					if err != nil {
						return
					}
					requests <- raw
					n := calls.Add(1)
					if n == 2 {
						if mode == "read_retry" {
							return
						}
						if mode == "missing_tool" || mode == "failed_missing_tool" {
							event := `{"type":"error","error":{"type":"invalid_request_error","param":"input","message":"No tool output found for function call call_lookup."}}`
							if mode == "failed_missing_tool" {
								event = `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"type":"invalid_request_error","param":"input","message":"No tool output found for function call call_lookup."}}}`
							}
							_ = conn.WriteMessage(websocket.TextMessage, []byte(event))
							return
						}
						if mode == "failed_anchor_miss" {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"id":"resp_failed","error":{"code":"previous_response_not_found","message":"previous response not found"}}}`))
							return
						}
						code := "previous_response_not_found"
						if mode == "http_fallback" {
							code = "websocket_not_supported"
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"error","error":{"code":%q,"type":"invalid_request_error","message":%q}}`, code, code)))
						return
					}
					output := autoContinuationAnswer
					if n == 1 {
						output = autoContinuationCall
					}
					if conn.WriteMessage(websocket.TextMessage, autoContinuationEvent(fmt.Sprintf("resp_%d", n), output)) != nil {
						return
					}
				}
			}))
			defer server.Close()
			s, a, httpUpstream := httpBridgeFixture(t, server.URL)
			defer s.openaiWSPool.Close()
			if mode == "http_fallback" {
				httpUpstream.responses[0] = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(autoContinuationEvent("resp_http", autoContinuationAnswer)) + "\n\n"))}
			}
			first, _ := httpBridgeContext("recover-incremental")
			_, err := s.Forward(context.Background(), first, a, autoContinuationBody(true))
			require.NoError(t, err)
			<-requests
			second, _ := httpBridgeContext("recover-incremental")
			_, err = s.Forward(context.Background(), second, a, autoContinuationBody(true, autoContinuationCall, autoContinuationTool))
			require.NoError(t, err)
			if mode != "http_fallback" {
				require.Equal(t, int32(3), calls.Load(), "one delta followed by one full replay")
				require.Equal(t, mode != "read_retry", second.GetBool("openai_ws_auto_incremental_replay"))
			}
			require.True(t, second.GetBool("openai_ws_auto_incremental_attempted"), "later attempts retain evidence that this request actually applied a delta")
			require.False(t, second.GetBool("openai_ws_auto_incremental"), "the final full request is not a delta")
			require.False(t, second.GetBool("openai_ws_auto_incremental_succeeded"), "a full replay is not a successful delta")
			if mode == "http_fallback" {
				require.Equal(t, "http_fallback", second.GetString("openai_ws_auto_incremental_reason"))
			}
			delta := <-requests
			require.Equal(t, "resp_1", gjson.GetBytes(delta, "previous_response_id").String())
			require.Len(t, gjson.GetBytes(delta, "input").Array(), 1)
			var replay []byte
			if mode == "http_fallback" {
				require.Equal(t, 1, httpUpstream.callCount)
				replay = httpUpstream.bodies[0]
			} else {
				require.Zero(t, httpUpstream.callCount)
				replay = <-requests
			}
			require.False(t, gjson.GetBytes(replay, "previous_response_id").Exists())
			require.Len(t, gjson.GetBytes(replay, "input").Array(), 4)
			require.Equal(t, "call_lookup", gjson.GetBytes(replay, "input.2.call_id").String())
			require.Equal(t, "call_lookup", gjson.GetBytes(replay, "input.3.call_id").String())
			require.Equal(t, "xhigh", gjson.GetBytes(replay, "reasoning.effort").String())
			if mode != "read_retry" {
				require.Equal(t, "reasoning.encrypted_content", gjson.GetBytes(replay, "include.0").String())
			}
		})
	}
}

func TestHTTPToWSAutoContinuationConnectionAndTenantBoundaries(t *testing.T) {
	for _, mode := range []string{"expired", "closed", "other_account", "other_group", "other_key", "other_thread"} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan []byte, 4)
			var dials atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				dials.Add(1)
				for {
					_, raw, err := conn.ReadMessage()
					if err != nil {
						return
					}
					requests <- raw
					if conn.WriteMessage(websocket.TextMessage, autoContinuationEvent("resp_boundary", autoContinuationCall)) != nil {
						return
					}
				}
			}))
			defer server.Close()
			s, a, _ := httpBridgeFixture(t, server.URL)
			defer s.openaiWSPool.Close()
			first, _ := httpBridgeContext("boundary-session")
			first.Request.Header.Set("thread-id", "thread-a")
			_, err := s.Forward(context.Background(), first, a, autoContinuationBody(true))
			require.NoError(t, err)
			<-requests
			poolValue, ok := s.openaiWSPool.accounts.Load(a.ID)
			require.True(t, ok)
			accountPool := poolValue.(*openAIWSAccountPool)
			accountPool.mu.Lock()
			var original *openAIWSConn
			for _, conn := range accountPool.conns {
				original = conn
				break
			}
			accountPool.mu.Unlock()
			require.NotNil(t, original)
			require.NotNil(t, original.deltaCheckpoint.Load())
			second, _ := httpBridgeContext("boundary-session")
			second.Request.Header.Set("thread-id", "thread-a")
			switch mode {
			case "expired":
				checkpoint := *original.deltaCheckpoint.Load()
				checkpoint.UpdatedUnixNano = time.Now().Add(-openAIWSDeltaCheckpointTTL - time.Second).UnixNano()
				original.deltaCheckpoint.Store(&checkpoint)
			case "closed":
				original.abort()
			case "other_account":
				other := *a
				other.ID = 24
				a = &other
			case "other_group":
				groupID := int64(98)
				second.Set("api_key", &APIKey{ID: 123, GroupID: &groupID})
			case "other_key":
				second.Set("api_key", &APIKey{ID: 456})
			case "other_thread":
				second.Request.Header.Set("thread-id", "thread-b")
			}
			_, err = s.Forward(context.Background(), second, a, autoContinuationBody(true, autoContinuationCall, autoContinuationTool))
			require.NoError(t, err)
			raw := <-requests
			require.False(t, second.GetBool("openai_ws_auto_incremental"))
			require.False(t, gjson.GetBytes(raw, "previous_response_id").Exists())
			require.Len(t, gjson.GetBytes(raw, "input").Array(), 4)
			if mode == "closed" {
				require.Equal(t, int32(2), dials.Load())
			}
		})
	}
}

func TestHTTPToWSAutoContinuationConcurrentBranches(t *testing.T) {
	requests := make(chan []byte, 4)
	secondArrived, releaseSecond := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseSecond) }) }
	defer unblock()
	var turns atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			requests <- raw
			n := turns.Add(1)
			if n == 2 {
				close(secondArrived)
				<-releaseSecond
			}
			output := autoContinuationAnswer
			if n == 1 {
				output = autoContinuationCall
			}
			if conn.WriteMessage(websocket.TextMessage, autoContinuationEvent(fmt.Sprintf("resp_branch_%d", n), output)) != nil {
				return
			}
		}
	}))
	defer server.Close()
	s, a, _ := httpBridgeFixture(t, server.URL)
	s.cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	s.cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	defer s.openaiWSPool.Close()
	first, _ := httpBridgeContext("branch-session")
	_, err := s.Forward(context.Background(), first, a, autoContinuationBody(true))
	require.NoError(t, err)
	<-requests
	done := make(chan error, 2)
	call := func(output string) {
		c, _ := httpBridgeContext("branch-session")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := s.Forward(ctx, c, a, autoContinuationBody(true, autoContinuationCall, output))
		done <- err
	}
	go call(autoContinuationTool)
	select {
	case <-secondArrived:
	case <-time.After(5 * time.Second):
		t.Fatal("second turn did not reach upstream")
	}
	go call(`{"type":"function_call_output","call_id":"call_lookup","output":"different branch result"}`)
	unblock()
	require.NoError(t, <-done)
	require.NoError(t, <-done)
	delta, full := <-requests, <-requests
	require.Equal(t, "resp_branch_1", gjson.GetBytes(delta, "previous_response_id").String())
	require.Len(t, gjson.GetBytes(delta, "input").Array(), 1)
	require.False(t, gjson.GetBytes(full, "previous_response_id").Exists(), "a second branch must not inherit the first branch's context")
	require.Len(t, gjson.GetBytes(full, "input").Array(), 4)
}

func TestHTTPToWSCheckpointPrewarmEmptyTail(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprint(exact), func(t *testing.T) {
			frames := make(chan []byte, 3)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for i := 0; i < 2; i++ {
					_, frame, err := conn.ReadMessage()
					if err != nil {
						return
					}
					frames <- frame
					output := ""
					id := "resp_warm"
					if i > 0 {
						output = autoContinuationAnswer
						id = "resp_turn"
					}
					if conn.WriteMessage(websocket.TextMessage, autoContinuationEvent(id, output)) != nil {
						return
					}
				}
				// Keep the socket alive until the client pool closes it.
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			svc, account, _ := httpBridgeFixture(t, server.URL)
			defer svc.openaiWSPool.Close()
			svc.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = true
			svc.cfg.Gateway.OpenAIWS.ExactFidelityEnabled = exact
			// This test covers frame serialization and prewarm, not TLS. Keep
			// the injected loopback dialer in the pool; the real exact transport
			// is covered separately by the network-isolated traffic acceptance.
			poolConfig := *svc.cfg
			poolConfig.Gateway.OpenAIWS.ExactFidelityEnabled = false
			svc.openaiWSPool.cfg = &poolConfig
			c, _ := httpBridgeContext("empty-tail")
			c.Request.Header.Set("User-Agent", "synthetic-compatible-client")
			_, err := svc.Forward(context.Background(), c, account, autoContinuationBody(false))
			require.NoError(t, err)
			warm, turn := <-frames, <-frames
			require.False(t, gjson.GetBytes(warm, "generate").Bool())
			require.True(t, gjson.GetBytes(warm, "generate").Exists())
			require.NotEmpty(t, gjson.GetBytes(warm, "input").Array())
			require.Equal(t, "resp_warm", gjson.GetBytes(turn, "previous_response_id").String())
			require.Equal(t, "[]", gjson.GetBytes(turn, "input").Raw)
		})
	}
}
