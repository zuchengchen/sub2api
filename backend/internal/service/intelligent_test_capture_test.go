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
