package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func nativeCaptureJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
func nativeCaptureResponseFixture() ([]byte, []byte) {
	output := []any{
		map[string]any{"id": "rs_capture", "type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-capture"},
		map[string]any{"id": "msg_capture", "type": "message", "role": "assistant", "phase": "commentary", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "visible answer"}}},
		map[string]any{"id": "fc_capture", "type": "function_call", "call_id": "call_capture", "name": "lookup", "arguments": `{"value":"synthetic"}`, "status": "completed"},
	}
	response := nativeCaptureJSON(map[string]any{"id": "resp_capture", "model": "gpt-6-astra", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 101, "output_tokens": 17}})
	var wire bytes.Buffer
	emit := func(v any) { wire.WriteString("data: "); wire.Write(nativeCaptureJSON(v)); wire.WriteString("\n\n") }
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_capture", "model": "gpt-6-astra"}})
	for index, item := range output {
		emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
		if index == 1 {
			emit(map[string]any{"type": "response.output_text.delta", "output_index": index, "content_index": 0, "delta": "visible answer"})
		}
		if index == 2 {
			emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "delta": `{"value":"synthetic"}`})
		}
		emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	}
	emit(map[string]any{"type": "response.completed", "response": json.RawMessage(response)})
	wire.WriteString("data: [DONE]\n\n")
	return wire.Bytes(), response
}

type nativeCaptureFragmentReader struct {
	reader io.Reader
	size   int
}

func (r *nativeCaptureFragmentReader) Read(p []byte) (int, error) {
	if len(p) > r.size {
		p = p[:r.size]
	}
	return r.reader.Read(p)
}
func (*nativeCaptureFragmentReader) Close() error { return nil }

func TestNativeReasoningReplayResponseCaptureFragmentationAndEventTypes(t *testing.T) {
	wire, response := nativeCaptureResponseFixture()
	for _, size := range []int{1, 7, 127, len(wire)} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			capture := NewNativeReasoningResponseCapture(&nativeCaptureFragmentReader{reader: bytes.NewReader(wire), size: size}, DefaultNativeReasoningReplayLimits())
			received, err := io.ReadAll(capture)
			require.NoError(t, err)
			require.Equal(t, wire, received)
			raw, ok := capture.CompletedResponse()
			require.True(t, ok)
			require.JSONEq(t, string(response), string(raw))
			raw[0] = '!'
			fresh, ok := capture.CompletedResponse()
			require.True(t, ok)
			require.JSONEq(t, string(response), string(fresh))
			require.NoError(t, capture.Close())
			after, ok := capture.CompletedResponse()
			require.True(t, ok)
			require.Equal(t, fresh, after)
		})
	}
	// The JSON type can be supplied by the SSE event field, and the document
	// can span multiple data lines and CRLF, LF or CR line endings.
	for _, eol := range []string{"\n", "\r\n", "\r"} {
		t.Run(fmt.Sprintf("multiline_%q", eol), func(t *testing.T) {
			var pretty bytes.Buffer
			require.NoError(t, json.Indent(&pretty, nativeCaptureJSON(map[string]any{"response": json.RawMessage(response)}), "", "  "))
			raw := []byte("\xef\xbb\xbfevent: response.completed" + eol + "data: " + strings.ReplaceAll(pretty.String(), "\n", eol+"data: ") + eol + eol)
			capture := NewNativeReasoningResponseCapture(&nativeCaptureFragmentReader{reader: bytes.NewReader(raw), size: 1}, DefaultNativeReasoningReplayLimits())
			out, err := io.ReadAll(capture)
			require.NoError(t, err)
			require.Equal(t, raw, out)
			got, ok := capture.CompletedResponse()
			require.True(t, ok)
			require.JSONEq(t, string(response), string(got))
		})
	}
}

func TestNativeReasoningReplayResponseCaptureRejectsIncompleteAndLimits(t *testing.T) {
	wire, response := nativeCaptureResponseFixture()
	for _, tc := range []struct {
		name   string
		wire   []byte
		limits NativeReasoningReplayLimits
	}{
		{"half frame", []byte("data: " + string(nativeCaptureJSON(map[string]any{"type": "response.completed", "response": json.RawMessage(response)}))), DefaultNativeReasoningReplayLimits()},
		{"missing terminal", []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_capture\"}}\n\n"), DefaultNativeReasoningReplayLimits()},
		{"bad JSON", append([]byte("data: {broken}\n\n"), wire...), DefaultNativeReasoningReplayLimits()},
		{"duplicate field", []byte("data: {\"type\":\"response.completed\",\"type\":\"response.completed\",\"response\":" + string(response) + "}\n\n"), DefaultNativeReasoningReplayLimits()},
		{"type conflict", []byte("event: response.failed\ndata: {\"type\":\"response.completed\",\"response\":" + string(response) + "}\n\n"), DefaultNativeReasoningReplayLimits()},
		{"line bound", wire, NativeReasoningReplayLimits{MaxEntryBytes: 80}},
		{"total wire bound", append([]byte(strings.Repeat(": heartbeat\n", 1000)), wire...), NativeReasoningReplayLimits{MaxHistoryBytes: 4096}},
		{"event bound", []byte(strings.Repeat("data: [DONE]\n\n", nativeReasoningCaptureMaxFrames+1) + string(wire)), DefaultNativeReasoningReplayLimits()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := NewNativeReasoningResponseCapture(io.NopCloser(bytes.NewReader(tc.wire)), tc.limits)
			got, err := io.ReadAll(capture)
			require.NoError(t, err)
			require.Equal(t, tc.wire, got)
			_, ok := capture.CompletedResponse()
			require.False(t, ok)
			require.NoError(t, capture.Close())
			require.Nil(t, capture.collector)
			require.Nil(t, capture.parser.line)
			require.Nil(t, capture.parser.data)
		})
	}
}

type nativeCaptureBlockedBody struct {
	started, closed   chan struct{}
	once              sync.Once
	late              []byte
	readErr, closeErr error
}

func (b *nativeCaptureBlockedBody) Read(p []byte) (int, error) {
	close(b.started)
	<-b.closed
	return copy(p, b.late), b.readErr
}
func (b *nativeCaptureBlockedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return b.closeErr
}
func TestNativeReasoningReplayResponseCaptureConcurrentClosePreservesErrors(t *testing.T) {
	wire, _ := nativeCaptureResponseFixture()
	readError, closeError := errors.New("synthetic read error"), errors.New("synthetic close error")
	body := &nativeCaptureBlockedBody{started: make(chan struct{}), closed: make(chan struct{}), late: wire, readErr: readError, closeErr: closeError}
	capture := NewNativeReasoningResponseCapture(body, DefaultNativeReasoningReplayLimits())
	done := make(chan struct{})
	var got []byte
	var gotErr error
	go func() {
		defer close(done)
		buffer := make([]byte, len(wire))
		n, err := capture.Read(buffer)
		got = append([]byte(nil), buffer[:n]...)
		gotErr = err
	}()
	<-body.started
	require.ErrorIs(t, capture.Close(), closeError)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close could not unblock Read")
	}
	require.Equal(t, wire, got)
	require.ErrorIs(t, gotErr, readError)
	_, ok := capture.CompletedResponse()
	require.False(t, ok)
	require.Nil(t, capture.completed)
	require.Nil(t, capture.collector)
	require.Nil(t, capture.parser.data)
	require.Nil(t, capture.parser.line)
}

type nativeCaptureErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *nativeCaptureErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), r.err
}
func (*nativeCaptureErrorReader) Close() error { return nil }
func TestNativeReasoningReplayResponseCaptureCancellationAndExactErrorCode(t *testing.T) {
	wire, _ := nativeCaptureResponseFixture()
	wire = wire[:bytes.Index(wire, []byte(`"type":"response.completed"`))]
	capture := NewNativeReasoningResponseCapture(&nativeCaptureErrorReader{data: wire, err: context.Canceled}, DefaultNativeReasoningReplayLimits())
	buffer := make([]byte, len(wire))
	n, err := capture.Read(buffer)
	require.Equal(t, wire, buffer[:n])
	require.ErrorIs(t, err, context.Canceled)
	_, ok := capture.CompletedResponse()
	require.False(t, ok)
	for _, tc := range []struct {
		payload string
		want    bool
	}{
		{`{"type":"error","error":{"code":"invalid_encrypted_content","message":"synthetic"}}`, true},
		{`{"type":"response.failed","response":{"error":{"code":"invalid_encrypted_content"}}}`, true},
		{`{"type":"error","error":{"code":"other","message":"invalid_encrypted_content"}}`, false},
		{`{"type":"response.output_text.delta","delta":"{\"error\":{\"code\":\"invalid_encrypted_content\"}}"}`, false},
		{`{"type":"error","error":{"code":"other","code":"invalid_encrypted_content"}}`, false},
		{`{"type":"error","error":{"code":"invalid_encrypted_content "}}`, false},
	} {
		c := NewNativeReasoningResponseCapture(io.NopCloser(strings.NewReader("data: "+tc.payload+"\n\n")), DefaultNativeReasoningReplayLimits())
		_, err := io.Copy(io.Discard, c)
		require.NoError(t, err)
		require.Equal(t, tc.want, c.InvalidEncryptedContent(), tc.payload)
		_, ok := c.CompletedResponse()
		require.False(t, ok)
	}
}

func nativeCaptureChatChunk(delta any, finish any) []byte {
	return nativeCaptureJSON(map[string]any{"id": "chatcmpl_capture", "object": "chat.completion.chunk", "model": "gpt-6-astra", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
}
func nativeCaptureChatWire() []byte {
	var wire bytes.Buffer
	emit := func(delta any, finish any) {
		wire.WriteString("data: ")
		wire.Write(nativeCaptureChatChunk(delta, finish))
		wire.WriteString("\n\n")
	}
	emit(map[string]any{"role": "assistant"}, nil)
	emit(map[string]any{"content": "visible \"answer\"\n", "reasoning_content": "summary", "refusal": "synthetic refusal"}, nil)
	emit(map[string]any{"tool_calls": []any{map[string]any{"index": 1, "id": "call_b", "type": "function", "function": map[string]any{"name": "beta", "arguments": "{"}}, map[string]any{"index": 0, "id": "call_a", "type": "function", "function": map[string]any{"name": "al", "arguments": "{"}}}}, nil)
	emit(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_a", "function": map[string]any{"name": "pha", "arguments": "}"}}, map[string]any{"index": 1, "function": map[string]any{"arguments": "}"}}}}, nil)
	emit(map[string]any{}, "tool_calls")
	wire.WriteString("data: [DONE]\n\n")
	return wire.Bytes()
}
func nativeCaptureTestWriter() (*httptest.ResponseRecorder, gin.ResponseWriter) {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	return rec, ctx.Writer
}

func TestNativeReasoningReplayChatCaptureRealBytesAndFragmentedMessage(t *testing.T) {
	wire := nativeCaptureChatWire()
	for _, size := range []int{1, 13, len(wire)} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rec, writer := nativeCaptureTestWriter()
			capture := NewNativeReasoningChatCapture(writer, true, DefaultNativeReasoningReplayLimits())
			for offset := 0; offset < len(wire); {
				end := min(offset+size, len(wire))
				var n int
				var err error
				if offset%2 == 0 {
					n, err = capture.Write(wire[offset:end])
				} else {
					n, err = capture.WriteString(string(wire[offset:end]))
				}
				require.NoError(t, err)
				require.Equal(t, end-offset, n)
				offset = end
			}
			require.Equal(t, wire, rec.Body.Bytes())
			raw, ok := capture.DeliveredAssistant()
			require.True(t, ok)
			var message apicompat.ChatMessage
			require.NoError(t, json.Unmarshal(raw, &message))
			require.Equal(t, "assistant", message.Role)
			require.JSONEq(t, `"visible \"answer\"\n"`, string(message.Content))
			require.Equal(t, "summary", message.ReasoningContent)
			require.Equal(t, "synthetic refusal", message.Refusal)
			require.Len(t, message.ToolCalls, 2)
			require.Equal(t, "alpha", message.ToolCalls[0].Function.Name)
			require.Equal(t, "beta", message.ToolCalls[1].Function.Name)
			require.Equal(t, "{}", message.ToolCalls[0].Function.Arguments)
			require.Nil(t, message.ToolCalls[0].Index)
			raw[0] = '!'
			fresh, ok := capture.DeliveredAssistant()
			require.True(t, ok)
			require.True(t, json.Valid(fresh))
		})
	}
}

func TestNativeReasoningReplayChatCaptureRejectsConflictsAndIncomplete(t *testing.T) {
	base := nativeCaptureChatWire()
	mutate := func(before, after string) []byte { return bytes.ReplaceAll(base, []byte(before), []byte(after)) }
	for _, tc := range []struct {
		name   string
		wire   []byte
		limits NativeReasoningReplayLimits
	}{
		{"missing DONE", bytes.TrimSuffix(base, []byte("data: [DONE]\n\n")), DefaultNativeReasoningReplayLimits()},
		{"half terminal", base[:len(base)-1], DefaultNativeReasoningReplayLimits()},
		{"missing role", mutate(`"role":"assistant"`, `"role":"user"`), DefaultNativeReasoningReplayLimits()},
		{"invalid choice index", mutate(`"index":0`, `"index":-1`), DefaultNativeReasoningReplayLimits()},
		{"conflicting tool ID", bytes.Replace(base, []byte(`"id":"call_a"`), []byte(`"id":"call_other"`), 1), DefaultNativeReasoningReplayLimits()},
		{"duplicate call ID", mutate(`"id":"call_b"`, `"id":"call_a"`), DefaultNativeReasoningReplayLimits()},
		{"missing tool index", mutate(`"index":1`, `"index":9`), DefaultNativeReasoningReplayLimits()},
		{"null tool index", mutate(`"index":1`, `"index":null`), DefaultNativeReasoningReplayLimits()},
		{"incomplete finish", mutate(`"finish_reason":"tool_calls"`, `"finish_reason":"length"`), DefaultNativeReasoningReplayLimits()},
		{"unknown delta", mutate(`"role":"assistant"`, `"role":"assistant","function_call":{"name":"other"}`), DefaultNativeReasoningReplayLimits()},
		{"duplicate JSON key", mutate(`"role":"assistant"`, `"role":"assistant","role":"assistant"`), DefaultNativeReasoningReplayLimits()},
		{"trailing data", append(append([]byte(nil), base...), []byte("data: {}\n\n")...), DefaultNativeReasoningReplayLimits()},
		{"logical size", base, NativeReasoningReplayLimits{MaxEntryBytes: 40}},
		{"wire size", append([]byte(strings.Repeat(": heartbeat\n", 1000)), base...), NativeReasoningReplayLimits{MaxHistoryBytes: 4096}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, writer := nativeCaptureTestWriter()
			capture := NewNativeReasoningChatCapture(writer, true, tc.limits)
			n, err := capture.Write(tc.wire)
			require.NoError(t, err)
			require.Equal(t, len(tc.wire), n)
			require.Equal(t, tc.wire, rec.Body.Bytes())
			_, ok := capture.DeliveredAssistant()
			require.False(t, ok)
		})
	}
}

type nativeCaptureFaultWriter struct {
	gin.ResponseWriter
	short      bool
	writeError error
}

func (w *nativeCaptureFaultWriter) Write(p []byte) (int, error) {
	if w.short && len(p) > 0 {
		p = p[:len(p)-1]
	}
	n, _ := w.ResponseWriter.Write(p)
	return n, w.writeError
}
func (w *nativeCaptureFaultWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func TestNativeReasoningReplayChatCaptureShortWritesAndErrors(t *testing.T) {
	wire := nativeCaptureChatWire()
	for _, stringWrite := range []bool{false, true} {
		for _, short := range []bool{false, true} {
			rec, base := nativeCaptureTestWriter()
			writeError := errors.New("synthetic write error")
			writer := &nativeCaptureFaultWriter{ResponseWriter: base, short: short}
			if !short {
				writer.writeError = writeError
			}
			capture := NewNativeReasoningChatCapture(writer, true, DefaultNativeReasoningReplayLimits())
			var n int
			var err error
			if stringWrite {
				n, err = capture.WriteString(string(wire))
			} else {
				n, err = capture.Write(wire)
			}
			expected := len(wire)
			if short {
				expected--
			}
			require.Equal(t, expected, n)
			require.Equal(t, wire[:expected], rec.Body.Bytes())
			if !short {
				require.ErrorIs(t, err, writeError)
			}
			_, ok := capture.DeliveredAssistant()
			require.False(t, ok)
			require.Nil(t, capture.parser.data)
			require.Nil(t, capture.chat.tools)
		}
	}
}

func TestNativeReasoningReplayChatCaptureBufferedAndConcurrentSnapshot(t *testing.T) {
	message := apicompat.ChatMessage{Role: "assistant", Content: json.RawMessage(`"visible"`), ToolCalls: []apicompat.ChatToolCall{{ID: "call_a", Type: "function", Function: apicompat.ChatFunctionCall{Name: "lookup", Arguments: "raw custom tool input"}}}}
	wire := nativeCaptureJSON(apicompat.ChatCompletionsResponse{ID: "chatcmpl_buffered", Object: "chat.completion", Choices: []apicompat.ChatChoice{{Index: 0, Message: message, FinishReason: "tool_calls"}}})
	rec, writer := nativeCaptureTestWriter()
	capture := NewNativeReasoningChatCapture(writer, false, DefaultNativeReasoningReplayLimits())
	for _, b := range wire {
		_, err := capture.Write([]byte{b})
		require.NoError(t, err)
	}
	require.Equal(t, wire, rec.Body.Bytes())
	raw, ok := capture.DeliveredAssistant()
	require.True(t, ok)
	require.JSONEq(t, string(nativeCaptureJSON(message)), string(raw))
	_, err := capture.Write([]byte(`{}`))
	require.NoError(t, err)
	_, ok = capture.DeliveredAssistant()
	require.False(t, ok)
	_, writer = nativeCaptureTestWriter()
	capture = NewNativeReasoningChatCapture(writer, true, DefaultNativeReasoningReplayLimits())
	_, err = capture.Write(nativeCaptureChatWire())
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				_, _ = capture.Write([]byte(": heartbeat\n\n"))
				capture.Flush()
				_, _ = capture.DeliveredAssistant()
			}
		}()
	}
	wg.Wait()
	_, ok = capture.DeliveredAssistant()
	require.True(t, ok)
}

// Both adapters are installed around the actual public production Chat bridge.
// This proves the captured client message is what the real converter wrote,
// without precomputing that message with a separate conversion helper.
func TestNativeReasoningReplayCaptureProductionChatRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			wire, response := nativeCaptureResponseFixture()
			native := NewNativeReasoningResponseCapture(&nativeCaptureFragmentReader{reader: bytes.NewReader(wire), size: 19}, DefaultNativeReasoningReplayLimits())
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: native}}
			svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
			account := newCodexSynthTestAccount(9901, AccountTypeOAuth)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			chat := NewNativeReasoningChatCapture(c.Writer, stream, DefaultNativeReasoningReplayLimits())
			c.Writer = chat
			body := nativeCaptureJSON(map[string]any{"model": "gpt-6-astra", "messages": []any{map[string]any{"role": "user", "content": "Synthetic capture round trip."}}, "stream": stream, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}})
			result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 101, result.Usage.InputTokens)
			require.Equal(t, 17, result.Usage.OutputTokens)
			original, ok := native.CompletedResponse()
			require.True(t, ok, "terminal boundary=%d, reader fragment=19, known sentinel bytes prefetched=%d", bytes.Index(wire, []byte("data: [DONE]")), (19-bytes.Index(wire, []byte("data: [DONE]"))%19)%19)
			require.JSONEq(t, string(response), string(original))
			delivered, ok := chat.DeliveredAssistant()
			require.True(t, ok)
			var message apicompat.ChatMessage
			require.NoError(t, json.Unmarshal(delivered, &message))
			require.Len(t, message.ToolCalls, 1)
			require.Equal(t, "lookup", message.ToolCalls[0].Function.Name)
			require.Equal(t, "call_capture", message.ToolCalls[0].ID)
			require.JSONEq(t, `{"value":"synthetic"}`, message.ToolCalls[0].Function.Arguments)
			require.JSONEq(t, `"visible answer"`, string(message.Content))
			require.NotContains(t, rec.Body.String(), "opaque-capture")
		})
	}
}

// The fixed source consumer exits on Completed (codex-api/src/sse/responses.rs
// 667-675; core/session/turn.rs 3348-3402). Read-ahead after that frame is outside
// its logical response, regardless of whether the optional sentinel was read.
func TestNativeReasoningReplayResponseCaptureStopsAtLogicalTerminalAcrossEveryCut(t *testing.T) {
	wire, response := nativeCaptureResponseFixture()
	terminal := wire[:bytes.Index(wire, []byte("data: [DONE]"))]
	for _, tail := range []string{"data: [DONE]\n\n", "data: {broken}\n\n"} {
		raw := append(append([]byte(nil), terminal...), tail...)
		for cut := 1; cut <= len(raw); cut++ {
			capture := NewNativeReasoningResponseCapture(&nativeCaptureFragmentReader{reader: bytes.NewReader(raw), size: cut}, DefaultNativeReasoningReplayLimits())
			_, err := io.Copy(io.Discard, capture)
			require.NoError(t, err)
			got, ok := capture.CompletedResponse()
			require.True(t, ok, "cut=%d tail_bytes=%d", cut, len(tail))
			require.JSONEq(t, string(response), string(got))
			require.NoError(t, capture.Close())
		}
	}
	// Even a single large low-level read cannot charge unconsumed read-ahead
	// against this already complete logical response's observation budget.
	raw := append(append([]byte(nil), terminal...), bytes.Repeat([]byte{'x'}, 4<<20)...)
	capture := NewNativeReasoningResponseCapture(io.NopCloser(bytes.NewReader(raw)), DefaultNativeReasoningReplayLimits())
	buffer := make([]byte, len(raw))
	n, err := capture.Read(buffer)
	require.NoError(t, err)
	require.Equal(t, raw, buffer[:n])
	got, ok := capture.CompletedResponse()
	require.True(t, ok)
	require.JSONEq(t, string(response), string(got))
}

func TestNativeReasoningReplayChatCaptureLogicalLimitIndependentOfWire(t *testing.T) {
	var wire bytes.Buffer
	emit := func(delta any, finish any) {
		wire.WriteString("data: ")
		wire.Write(nativeCaptureChatChunk(delta, finish))
		wire.WriteString("\n\n")
	}
	emit(map[string]any{"role": "assistant"}, nil)
	for i := 0; i < 3; i++ {
		emit(map[string]any{"content": strings.Repeat("x", 200)}, nil)
	}
	emit(map[string]any{}, "stop")
	wire.WriteString("data: [DONE]\n\n")
	limits := NativeReasoningReplayLimits{MaxEntryBytes: 512}
	require.Less(t, wire.Len(), nativeReasoningCaptureWireLimit(limits.Normalize()))
	rec, writer := nativeCaptureTestWriter()
	capture := NewNativeReasoningChatCapture(writer, true, limits)
	n, err := capture.Write(wire.Bytes())
	require.NoError(t, err)
	require.Equal(t, wire.Len(), n)
	require.Equal(t, wire.Bytes(), rec.Body.Bytes())
	_, ok := capture.DeliveredAssistant()
	require.False(t, ok)
	require.True(t, capture.invalid)
}
