package service

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Event shapes captured from the ChatGPT Codex WS upstream (gpt-6-astra,
// 2026-10-23): response.completed carries output:[] while the answer only
// arrives in output_text.delta / output_item.done.
const (
	wsEmptyTerminalCreated   = `{"type":"response.created","response":{"id":"resp_ws_empty","object":"response","model":"gpt-6-astra","status":"in_progress","output":[]}}`
	wsEmptyTerminalItemAdded = `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_ws_empty","type":"message","status":"in_progress","role":"assistant","content":[]}}`
	wsEmptyTerminalDelta     = `{"type":"response.output_text.delta","item_id":"msg_ws_empty","output_index":0,"content_index":0,"delta":"15"}`
	wsEmptyTerminalItemDone  = `{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_ws_empty","type":"message","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"15"}]}}`
	wsEmptyTerminalCompleted = `{"type":"response.completed","response":{"id":"resp_ws_empty","object":"response","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":8820,"output_tokens":5,"input_tokens_details":{"cached_tokens":8704}}}}`
)

func runOpenAIWSV2TerminalOutputCase(t *testing.T, stream bool, events []string) (*OpenAIForwardResult, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	cfg := newOpenAIWSV2TestConfig()
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0

	raw := make([][]byte, 0, len(events))
	for _, event := range events {
		raw = append(raw, []byte(event))
	}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: &openAIWSCaptureConn{events: raw}})
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
	account := &Account{
		ID: 5891, Name: "openai-ws-v2-empty-terminal", Platform: PlatformOpenAI,
		Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}

	body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"Q: 9 + 6 = ?\nA:"}`, stream))
	result, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.OpenAIWSMode, "must take forwardOpenAIWSV2")
	return result, rec
}

func openAIWSV2SSEEvent(t *testing.T, body, eventType string) []byte {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if gjson.Get(data, "type").String() == eventType {
			return []byte(data)
		}
	}
	t.Fatalf("event %s not found in stream body: %s", eventType, body)
	return nil
}

func TestForwardOpenAIWSV2_NonStreamRebuildsEmptyTerminalOutput(t *testing.T) {
	result, rec := runOpenAIWSV2TerminalOutputCase(t, false, []string{
		wsEmptyTerminalCreated, wsEmptyTerminalItemAdded, wsEmptyTerminalDelta, wsEmptyTerminalItemDone, wsEmptyTerminalCompleted,
	})

	body := rec.Body.Bytes()
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "15", extractOpenAIResponsesText(body), "body=%s", body)
	output := gjson.GetBytes(body, "output").Array()
	require.Len(t, output, 1)
	require.Equal(t, "msg_ws_empty", output[0].Get("id").String(), "raw output_item.done item is preserved")
	require.Equal(t, "final_answer", output[0].Get("phase").String())
	require.Equal(t, "completed", gjson.GetBytes(body, "status").String())
	require.Equal(t, int64(5), gjson.GetBytes(body, "usage.output_tokens").Int())
	require.Equal(t, 5, result.Usage.OutputTokens, "usage accounting unchanged by the rebuild")
	require.Equal(t, 8820, result.Usage.InputTokens)
}

func TestForwardOpenAIWSV2_StreamRebuildsEmptyTerminalOutput(t *testing.T) {
	result, rec := runOpenAIWSV2TerminalOutputCase(t, true, []string{
		wsEmptyTerminalCreated, wsEmptyTerminalItemAdded, wsEmptyTerminalDelta, wsEmptyTerminalItemDone, wsEmptyTerminalCompleted,
	})

	completed := openAIWSV2SSEEvent(t, rec.Body.String(), "response.completed")
	require.Equal(t, "15", extractOpenAIResponsesText([]byte(gjson.GetBytes(completed, "response").Raw)))
	require.Len(t, gjson.GetBytes(completed, "response.output").Array(), 1)
	require.Equal(t, int64(5), gjson.GetBytes(completed, "response.usage.output_tokens").Int())
	require.Equal(t, 5, result.Usage.OutputTokens)
	// The delta stream itself is forwarded untouched.
	require.Equal(t, "15", gjson.GetBytes(openAIWSV2SSEEvent(t, rec.Body.String(), "response.output_text.delta"), "delta").String())
}

func TestForwardOpenAIWSV2_NonStreamRebuildsFromDeltasWithoutItemDone(t *testing.T) {
	_, rec := runOpenAIWSV2TerminalOutputCase(t, false, []string{
		wsEmptyTerminalCreated, wsEmptyTerminalItemAdded, wsEmptyTerminalDelta, wsEmptyTerminalCompleted,
	})

	body := rec.Body.Bytes()
	require.Equal(t, "15", extractOpenAIResponsesText(body), "body=%s", body)
}

func TestForwardOpenAIWSV2_NonStreamKeepsCompleteTerminalOutput(t *testing.T) {
	completeTerminal := `{"type":"response.completed","response":{"id":"resp_ws_empty","object":"response","model":"gpt-6-astra","status":"completed","output":[{"id":"msg_terminal","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"terminal-wins"}]}],"usage":{"input_tokens":1,"output_tokens":2}}}`
	_, rec := runOpenAIWSV2TerminalOutputCase(t, false, []string{
		wsEmptyTerminalCreated, wsEmptyTerminalItemAdded, wsEmptyTerminalDelta, wsEmptyTerminalItemDone, completeTerminal,
	})

	body := rec.Body.Bytes()
	output := gjson.GetBytes(body, "output").Array()
	require.Len(t, output, 1)
	require.Equal(t, "msg_terminal", output[0].Get("id").String(), "a complete terminal output is left untouched")
	require.Equal(t, "terminal-wins", extractOpenAIResponsesText(body))
}

func TestForwardOpenAIWSV2_NonStreamEmptyTerminalWithoutContentStaysEmpty(t *testing.T) {
	_, rec := runOpenAIWSV2TerminalOutputCase(t, false, []string{
		wsEmptyTerminalCreated, wsEmptyTerminalCompleted,
	})

	body := rec.Body.Bytes()
	require.True(t, gjson.GetBytes(body, "output").IsArray())
	require.Empty(t, gjson.GetBytes(body, "output").Array(), "nothing to rebuild: output stays []")
}
