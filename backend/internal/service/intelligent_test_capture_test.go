package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const pelicanCaptureTestHTML = `<!DOCTYPE html><html><body><svg viewBox="0 0 10 10"><circle cx="5" cy="5" r="4"><animate attributeName="r" values="3;5;3" dur="2s" repeatCount="indefinite"/></circle></svg></body></html>`

func paddedOpenAIDeltaSSE(t *testing.T, html string, minBytes int) string {
	t.Helper()
	n := len(html)
	if n < 1 {
		n = 1
	}
	pad := strings.Repeat("x", minBytes/n+32)
	var b strings.Builder
	b.Grow(minBytes + 256)
	for i := 0; i < len(html); i++ {
		delta, err := json.Marshal(html[i : i+1])
		require.NoError(t, err)
		fmt.Fprintf(&b, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%s,\"obfuscation\":%q,\"sequence_number\":%d}\n\n", delta, pad, i+1)
	}
	b.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	require.Greater(t, b.Len(), minBytes)
	return b.String()
}

func TestIntelligentCaptureReconstructsHTMLWhenRawSSEExceedsOneMB(t *testing.T) {
	t.Parallel()
	sse := paddedOpenAIDeltaSSE(t, pelicanCaptureTestHTML, 1<<20)
	capture := &intelligentCapture{}
	n, err := io.Copy(capture, strings.NewReader(sse))
	require.NoError(t, err)
	require.Equal(t, int64(len(sse)), n)
	require.Equal(t, pelicanCaptureTestHTML, capture.outputText())
	require.True(t, capture.upstreamComplete())
	require.False(t, capture.textTruncated)
}

func TestIntelligentCaptureReconstructsHTMLWhenRawSSEExceedsFourMB(t *testing.T) {
	t.Parallel()
	sse := paddedOpenAIDeltaSSE(t, pelicanCaptureTestHTML, 4<<20)
	capture := &intelligentCapture{}
	_, err := io.Copy(capture, strings.NewReader(sse))
	require.NoError(t, err)
	require.Equal(t, pelicanCaptureTestHTML, capture.outputText())
	require.True(t, capture.upstreamComplete())
}

func TestIntelligentCaptureResponseBodyKeepsReadingPastFourMB(t *testing.T) {
	t.Parallel()
	sse := paddedOpenAIDeltaSSE(t, pelicanCaptureTestHTML, 4<<20)
	capture := &intelligentCapture{}
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	require.NoError(t, err)
	resp := capture.response(req, &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse))})
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, len(sse), len(got), "4 MiB LimitReader must not cut a still-open pelican stream")
	require.Equal(t, pelicanCaptureTestHTML, capture.outputText())
	require.True(t, capture.upstreamComplete())
}

func TestIntelligentSSEWriterDoesNotCancelDeltaStormUnderTextCap(t *testing.T) {
	t.Parallel()
	var cancelled atomic.Bool
	writer := &intelligentSSEWriter{header: http.Header{}, cancel: func() { cancelled.Store(true) }}
	html := pelicanCaptureTestHTML
	pad := strings.Repeat("x", (2<<20)/len(html)+64)
	var payload strings.Builder
	payload.Grow(2<<20 + 1024)
	for i := 0; i < len(html); i++ {
		text, err := json.Marshal(html[i : i+1])
		require.NoError(t, err)
		fmt.Fprintf(&payload, "data: {\"type\":\"content\",\"text\":%s,\"padding\":%q}\n\n", text, pad)
	}
	fmt.Fprintf(&payload, "data: {\"type\":\"test_complete\",\"success\":true}\n\n")
	require.Greater(t, payload.Len(), 2<<20)

	n, err := writer.Write([]byte(payload.String()))
	require.NoError(t, err)
	require.Equal(t, payload.Len(), n)
	require.False(t, cancelled.Load(), "delta JSON size must not cancel the upstream read")
	require.False(t, writer.truncated)
	require.True(t, writer.sawComplete)
	require.Equal(t, html, writer.outputText())
}

func TestFinalizeIntelligentTestSucceedsWhenRawDebugTruncatedButHTMLComplete(t *testing.T) {
	t.Parallel()
	record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
	capture := &intelligentCapture{truncated: true, complete: true, textTruncated: false}
	capture.text.WriteString(pelicanCaptureTestHTML)
	writer := &intelligentSSEWriter{header: http.Header{}, cancel: func() {}, sawComplete: true}
	writer.text.WriteString(pelicanCaptureTestHTML)
	err := finalizeIntelligentTestRun(record, capture, writer, pelicanCaptureTestHTML, "", "", nil)
	require.NoError(t, err)
	require.Empty(t, record.Status)
	require.Equal(t, pelicanCaptureTestHTML, record.Result)
	require.True(t, record.RawTruncated)
}

func TestFinalizeIntelligentTestStillFailsWithoutCompletedEvent(t *testing.T) {
	t.Parallel()
	record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
	capture := &intelligentCapture{complete: false}
	capture.text.WriteString(pelicanCaptureTestHTML)
	writer := &intelligentSSEWriter{header: http.Header{}, cancel: func() {}}
	err := finalizeIntelligentTestRun(record, capture, writer, pelicanCaptureTestHTML, "", "", nil)
	require.Error(t, err)
	require.Equal(t, "failed", record.Status)
}

func TestIntelligentCaptureIgnoresContextCancelFromWriterSize(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = ctx
	sse := paddedOpenAIDeltaSSE(t, pelicanCaptureTestHTML, 1<<20)
	capture := &intelligentCapture{}
	_, err := capture.Write([]byte(sse))
	require.NoError(t, err)
	require.True(t, capture.upstreamComplete())
}

func writeIntelligentCaptureEvent(t *testing.T, capture *intelligentCapture, event map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	_, err = capture.Write(append(append([]byte("data: "), encoded...), '\n', '\n'))
	require.NoError(t, err)
}

func intelligentCaptureCompletion(kind, text string) map[string]any {
	return map[string]any{"type": kind, "response": map[string]any{
		"status": "completed", "model": "gpt-6-astra",
		"output": []any{map[string]any{"type": "message", "status": "completed", "content": []any{
			map[string]any{"type": "output_text", "text": text},
		}}},
	}}
}

func TestIntelligentCaptureFinalOutputReplacesDeltasWithoutDuplication(t *testing.T) {
	for _, kind := range []string{"response.completed", "response.done"} {
		for _, delta := range []string{"", "<!DOCTYPE html><html>", pelicanCaptureTestHTML, "stale intermediate text"} {
			t.Run(fmt.Sprintf("%s_delta_%d", kind, len(delta)), func(t *testing.T) {
				capture := &intelligentCapture{}
				if delta != "" {
					writeIntelligentCaptureEvent(t, capture, map[string]any{"type": "response.output_text.delta", "delta": delta})
				}
				writeIntelligentCaptureEvent(t, capture, intelligentCaptureCompletion(kind, pelicanCaptureTestHTML))
				require.Equal(t, pelicanCaptureTestHTML, capture.outputText())
				require.True(t, capture.upstreamComplete())
				record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
				require.NoError(t, finalizeIntelligentTestRun(record, capture, nil, delta, "", "", nil))
				require.Equal(t, pelicanCaptureTestHTML, record.Result)
			})
		}
	}
}

func TestIntelligentCaptureFinalOutputUsesOrderedTextParts(t *testing.T) {
	capture := &intelligentCapture{}
	writeIntelligentCaptureEvent(t, capture, map[string]any{"type": "response.output_text.delta", "delta": "secondfirst"})
	writeIntelligentCaptureEvent(t, capture, map[string]any{"type": "response.completed", "response": map[string]any{
		"status": "completed", "output": []any{
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"text": "private reasoning"}}},
			map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "first"}, map[string]any{"type": "output_text", "text": " second"}}},
			map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": " third"}}},
		},
	}})
	require.Equal(t, "first second third", capture.outputText())
	require.True(t, capture.upstreamComplete())
}

func TestIntelligentCaptureEmptyFinalOutputClearsStaleDelta(t *testing.T) {
	capture := &intelligentCapture{}
	writeIntelligentCaptureEvent(t, capture, map[string]any{"type": "response.output_text.delta", "delta": pelicanCaptureTestHTML})
	writeIntelligentCaptureEvent(t, capture, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{}}})
	require.Empty(t, capture.outputText())
	writer := &intelligentSSEWriter{sawComplete: true}
	writer.text.WriteString(pelicanCaptureTestHTML)
	record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
	require.Error(t, finalizeIntelligentTestRun(record, capture, writer, pelicanCaptureTestHTML, "", "", nil))
	require.Empty(t, record.Result)
}

func TestIntelligentCaptureFailureCannotInheritWriterSuccess(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error"} {
		t.Run(kind, func(t *testing.T) {
			capture := &intelligentCapture{}
			writeIntelligentCaptureEvent(t, capture, intelligentCaptureCompletion("response.completed", pelicanCaptureTestHTML))
			writeIntelligentCaptureEvent(t, capture, map[string]any{"type": kind})
			require.False(t, capture.upstreamComplete())
			record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
			require.Error(t, finalizeIntelligentTestRun(record, capture, &intelligentSSEWriter{sawComplete: true}, "", "", "", nil))
			require.Equal(t, pelicanCaptureTestHTML, record.Result, "partial output remains available for diagnosis")
		})
	}
	for _, status := range []string{"incomplete", "failed", "cancelled", "in_progress"} {
		t.Run("completed_status_"+status, func(t *testing.T) {
			capture := &intelligentCapture{}
			event := intelligentCaptureCompletion("response.completed", pelicanCaptureTestHTML)
			event["response"].(map[string]any)["status"] = status
			writeIntelligentCaptureEvent(t, capture, event)
			require.False(t, capture.upstreamComplete())
		})
	}
}

func TestIntelligentCaptureFinalOutputHonorsTextLimit(t *testing.T) {
	for _, size := range []int{intelligentCaptureTextLimit, intelligentCaptureTextLimit + 1} {
		capture := &intelligentCapture{}
		writeIntelligentCaptureEvent(t, capture, intelligentCaptureCompletion("response.completed", strings.Repeat("a", size)))
		require.Len(t, capture.outputText(), intelligentCaptureTextLimit)
		require.Equal(t, size <= intelligentCaptureTextLimit, capture.upstreamComplete())
		require.Equal(t, size > intelligentCaptureTextLimit, capture.textTruncated)
	}
}

func TestIntelligentCaptureFailedTerminalOnlyRetainsDiagnosticOutput(t *testing.T) {
	for _, kind := range []string{"response.failed", "response.incomplete", "response.cancelled", "response.canceled"} {
		capture := &intelligentCapture{}
		event := intelligentCaptureCompletion(kind, "<html><svg>partial")
		event["response"].(map[string]any)["status"] = "incomplete"
		writeIntelligentCaptureEvent(t, capture, event)
		record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
		require.Error(t, finalizeIntelligentTestRun(record, capture, nil, "", "", "", nil))
		require.Equal(t, "<html><svg>partial", record.Result)
		require.False(t, capture.upstreamComplete())
	}
}
