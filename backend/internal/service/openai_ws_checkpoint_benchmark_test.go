package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

// Uses only APIs present at 0a59ad440, so the identical benchmark file can be
// overlaid onto that baseline. Both versions must send the same full request
// followed by exactly one new input item on the same response anchor.
// One operation is two complete forwarded responses, not a cached zero-work hit.
func BenchmarkOpenAIWSDeltaHTTPToWS(b *testing.B) {
	// A benchmark-only run has no ordinary test to initialize the atomic level.
	logger.InitBootstrap()
	beforeLevel := logger.CurrentLevel()
	if err := logger.SetLevel("error"); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = logger.SetLevel(beforeLevel) })
	for _, count := range []int{16, 128} {
		b.Run(fmt.Sprintf("history_%d", count), func(b *testing.B) {
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.DeltaContextEnabled = true
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			capture := &openAIWSCaptureConn{}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: capture})
			b.Cleanup(pool.Close)
			svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool, openaiWSStateStore: NewOpenAIWSStateStore(nil), toolCorrector: NewCodexToolCorrector()}
			account := &Account{ID: 31, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1}
			items := make([]json.RawMessage, 0, count+2)
			for i := 0; i < count; i++ {
				raw, _ := json.Marshal(map[string]any{"type": "message", "id": fmt.Sprintf("msg_%d", i), "role": "user", "content": strings.Repeat("synthetic text ", 32)})
				items = append(items, raw)
			}
			output := json.RawMessage(`{"type":"message","id":"msg_output","role":"assistant","content":[{"type":"output_text","text":"ok"}]}`)
			user := json.RawMessage(`{"type":"message","id":"msg_next","role":"user","content":"next"}`)
			rawFirst, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "stream": false, "store": false, "input": items})
			rawSecond, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "stream": false, "store": false, "input": append(append([]json.RawMessage{}, items...), output, user)})
			done := []byte(`{"type":"response.output_item.done","output_index":0,"item":` + string(output) + `}`)
			completed := []byte(`{"type":"response.completed","response":{"id":"resp_benchmark","status":"completed","output":[` + string(output) + `],"usage":{"input_tokens":10,"output_tokens":1}}}`)
			secondCompleted := []byte(`{"type":"response.completed","response":{"id":"resp_second","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":1}}}`)
			inputCount := func(value any) int {
				switch v := value.(type) {
				case []json.RawMessage:
					return len(v)
				case []any:
					return len(v)
				}
				return -1
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				capture.events = [][]byte{done, completed, secondCompleted}
				capture.writes = nil
				for _, raw := range [][]byte{rawFirst, rawSecond} {
					var payload map[string]any
					if err := decodeOpenAIJSONUseNumber(raw, &payload); err != nil {
						b.Fatal(err)
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					c.Request.Header.Set("session-id", "benchmark-thread")
					c.Set("api_key", &APIKey{ID: 41})
					SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
					result, err := svc.forwardOpenAIWSV2(context.Background(), c, account, payload, raw, "", "benchmark-thread", "synthetic", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, false, false, "gpt-5.5", "gpt-5.5", time.Now(), 1, "", nil)
					if err != nil || result == nil || !result.SucceededForScheduling() {
						b.Fatalf("forwarded response failed: result=%v err=%v", result, err)
					}
				}
				if len(capture.writes) != 2 || len(capture.events) != 0 {
					b.Fatal("incomplete benchmark exchange")
				}
				if openAIWSPayloadString(capture.writes[0], "previous_response_id") != "" || inputCount(capture.writes[0]["input"]) != count {
					b.Fatal("first request must contain the complete history")
				}
				if openAIWSPayloadString(capture.writes[1], "previous_response_id") != "resp_benchmark" || inputCount(capture.writes[1]["input"]) != 1 {
					b.Fatal("second request must contain exactly the new user item")
				}
			}
			b.StopTimer()
			first, _ := json.Marshal(capture.writes[0])
			second, _ := json.Marshal(capture.writes[1])
			b.ReportMetric(float64(len(first)+len(second)), "upstream_B/op")
		})
	}
}
