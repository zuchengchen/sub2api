package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

var emptyCompletionReplayPaths = []string{"capture", "http-intelligent", "http-live", "ws-intelligent", "ws-live"}

func emptyCompletionReplayFixture(t *testing.T) []map[string]any {
	t.Helper()
	// Only protocol fields and synthetic item IDs remain in this sequence;
	// no account, request or private record data is stored in the fixture.
	raw, err := os.ReadFile("testdata/intelligent_empty_completion.sse")
	require.NoError(t, err)
	var events []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event))
		events = append(events, event)
	}
	require.Len(t, events, 10)
	return events
}

func replayEmptyCompletion(t *testing.T, path string, events []map[string]any) (*IntelligentTestRecord, *intelligentCapture, []TestEvent, error) {
	t.Helper()
	var stream strings.Builder
	var wsEvents [][]byte
	for _, event := range events {
		raw, err := json.Marshal(event)
		require.NoError(t, err)
		wsEvents = append(wsEvents, raw)
		stream.WriteString("data: " + string(raw) + "\n\n")
	}
	// Exercise the valid EOF tail in both capture and HTTP replay.
	sse := strings.TrimRight(stream.String(), "\n")
	capture := &intelligentCapture{}
	record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
	if path == "capture" {
		chunkSize := 7
		if len(sse) > 16<<10 {
			chunkSize = 4096
		}
		for pos := 0; pos < len(sse); {
			end := pos + chunkSize
			if end > len(sse) {
				end = len(sse)
			}
			_, err := capture.Write([]byte(sse[pos:end]))
			require.NoError(t, err)
			pos = end
		}
		err := finalizeIntelligentTestRun(record, capture, nil, "", "", "", nil)
		return record, capture, nil, err
	}
	writer := &intelligentSSEWriter{header: http.Header{}}
	c, _ := gin.CreateTestContext(writer)
	ctx := context.Background()
	intelligent := strings.HasSuffix(path, "intelligent")
	if intelligent {
		ctx = context.WithValue(ctx, intelligentRunKey{}, &intelligentRunContext{prompt: "Count the candies", testType: "candy", capture: capture})
	}
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil).WithContext(ctx)
	var adapterErr error
	if strings.HasPrefix(path, "http-") {
		body := io.TeeReader(strings.NewReader(sse), capture)
		adapterErr = (&AccountTestService{}).processOpenAIStream(c, body)
	} else {
		conn := &openAIWSCaptureConn{events: wsEvents}
		gateway, account, _, _ := newCookieForwardFixture(t, conn)
		svc := &AccountTestService{openaiGatewayService: gateway, httpUpstream: gateway.httpUpstream}
		adapterErr = svc.testOpenAICookieWSAccountConnection(c, account, openAICodexTicketDefaultModel, "Count the candies")
		if !intelligent {
			// Normal live tests expose TestEvents without a shared capture.
			capture = nil
		}
	}
	err := finalizeIntelligentTestRun(record, capture, writer, "", "", "", adapterErr)
	return record, capture, accountOpenAIStreamTestEvents(t, writer.body.String()), err
}

func emptyCompletionMessageDone(index any, text string) map[string]any {
	return map[string]any{
		"type": "response.output_item.done", "output_index": index,
		"item": map[string]any{
			"id": fmt.Sprintf("fixture_message_%v", index), "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		},
	}
}

func emptyCompletionTerminal(output []any) map[string]any {
	return map[string]any{
		"type":     "response.completed",
		"response": map[string]any{"model": openAICodexTicketDefaultModel, "status": "completed", "output": output},
	}
}

func TestIntelligentEmptyCompletionOrdersAndDeduplicatesCompletedItems(t *testing.T) {
	for _, path := range emptyCompletionReplayPaths {
		t.Run(path, func(t *testing.T) {
			last := emptyCompletionMessageDone(9, "third")
			last["item"].(map[string]any)["content"] = []any{
				map[string]any{"type": "output_text", "text": "thi"},
				map[string]any{"type": "refusal", "text": "must not appear"},
				map[string]any{"type": "output_text", "text": "rd"},
			}
			events := []map[string]any{
				emptyCompletionMessageDone(3, "replaced old content"),
				last,
				emptyCompletionMessageDone(1, "first "),
				emptyCompletionMessageDone(3, "second "),
				emptyCompletionMessageDone(3, "second "),
				emptyCompletionTerminal([]any{}),
			}
			record, _, emitted, err := replayEmptyCompletion(t, path, events)
			require.NoError(t, err)
			require.Equal(t, "first second third", record.Result)
			var output strings.Builder
			for _, event := range emitted {
				if event.Type == "content" {
					output.WriteString(event.Text)
				}
			}
			if path != "capture" {
				require.Equal(t, record.Result, output.String())
			}
		})
	}
}

func TestIntelligentEmptyCompletionNonemptyTerminalRemainsAuthoritative(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output []any
		want   string
	}{
		{"final_text", []any{emptyCompletionMessageDone(1, "final answer")["item"]}, "final answer"},
		{"reasoning_only", []any{map[string]any{"type": "reasoning", "status": "completed"}}, ""},
		{"empty_text", []any{emptyCompletionMessageDone(1, "")["item"]}, ""},
	} {
		for _, path := range emptyCompletionReplayPaths {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				record, _, _, err := replayEmptyCompletion(t, path, []map[string]any{
					emptyCompletionMessageDone(1, "stale done text"), emptyCompletionTerminal(tc.output),
				})
				if tc.want == "" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, tc.want, record.Result)
			})
		}
	}
}

func TestIntelligentEmptyCompletionRejectsUnfinishedOrInvalidItems(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"user_message", func(event map[string]any) { event["item"].(map[string]any)["role"] = "user" }},
		{"other_item_type", func(event map[string]any) { event["item"].(map[string]any)["type"] = "reasoning" }},
		{"missing_status", func(event map[string]any) { delete(event["item"].(map[string]any), "status") }},
		{"in_progress", func(event map[string]any) { event["item"].(map[string]any)["status"] = "in_progress" }},
		{"missing_index", func(event map[string]any) { delete(event, "output_index") }},
		{"negative_index", func(event map[string]any) { event["output_index"] = -1 }},
		{"fractional_index", func(event map[string]any) { event["output_index"] = 1.5 }},
		{"string_index", func(event map[string]any) { event["output_index"] = "1" }},
	} {
		for _, path := range []string{"capture", "http-intelligent", "ws-intelligent"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				item := emptyCompletionMessageDone(1, "unverified text")
				tc.mutate(item)
				record, _, _, err := replayEmptyCompletion(t, path, []map[string]any{item, emptyCompletionTerminal([]any{})})
				require.Error(t, err)
				require.Empty(t, record.Result)
			})
		}
	}
	for _, path := range []string{"capture", "http-intelligent", "ws-intelligent"} {
		t.Run("pending_message/"+path, func(t *testing.T) {
			pending := emptyCompletionMessageDone(3, "unfinished")
			pending["type"] = "response.output_item.added"
			pending["item"].(map[string]any)["status"] = "in_progress"
			record, _, _, err := replayEmptyCompletion(t, path, []map[string]any{
				emptyCompletionMessageDone(1, "first finished"), pending, emptyCompletionTerminal([]any{}),
			})
			require.Error(t, err)
			require.Empty(t, record.Result)
		})
	}
}

func TestIntelligentEmptyCompletionRequiresItemDoneOnlyForExplicitEmptyOutput(t *testing.T) {
	for _, path := range []string{"capture", "http-intelligent", "ws-intelligent"} {
		t.Run("text_and_part_done_are_insufficient/"+path, func(t *testing.T) {
			fixture := emptyCompletionReplayFixture(t)
			var events []map[string]any
			for _, event := range fixture {
				if event["type"] == "response.output_item.done" {
					continue
				}
				events = append(events, event)
			}
			record, _, _, err := replayEmptyCompletion(t, path, events)
			require.Error(t, err)
			require.Empty(t, record.Result)
		})
		t.Run("missing_output_keeps_delta_compatibility/"+path, func(t *testing.T) {
			terminal := emptyCompletionTerminal(nil)
			delete(terminal["response"].(map[string]any), "output")
			record, _, _, err := replayEmptyCompletion(t, path, []map[string]any{
				{"type": "response.output_text.delta", "delta": "legacy delta"}, terminal,
			})
			require.NoError(t, err)
			require.Equal(t, "legacy delta", record.Result)
		})
	}
}

func TestIntelligentEmptyCompletionCannotUpgradeFailureOrMissingTerminal(t *testing.T) {
	for _, tc := range []struct{ kind, status string }{
		{"response.failed", "failed"}, {"response.incomplete", "incomplete"},
		{"response.cancelled", "cancelled"}, {"response.canceled", "canceled"},
		{"response.completed", "incomplete"}, {"", ""},
	} {
		for _, path := range emptyCompletionReplayPaths {
			t.Run(tc.kind+tc.status+"/"+path, func(t *testing.T) {
				events := []map[string]any{
					{"type": "response.output_text.delta", "delta": "diagnostic partial"},
					emptyCompletionMessageDone(1, "finished item cannot establish response success"),
				}
				if tc.kind != "" {
					terminal := emptyCompletionTerminal([]any{})
					terminal["type"] = tc.kind
					terminal["response"].(map[string]any)["status"] = tc.status
					events = append(events, terminal)
				}
				record, capture, _, err := replayEmptyCompletion(t, path, events)
				require.Error(t, err)
				require.Equal(t, "diagnostic partial", record.Result)
				if capture != nil {
					require.False(t, capture.upstreamComplete())
				}
			})
		}
	}
}

func TestIntelligentEmptyCompletionRecoveryIsBounded(t *testing.T) {
	t.Run("text_limit_and_replacement", func(t *testing.T) {
		text := strings.Repeat("x", intelligentCaptureTextLimit)
		record, capture, _, err := replayEmptyCompletion(t, "capture", []map[string]any{
			emptyCompletionMessageDone(1, text), emptyCompletionMessageDone(1, text), emptyCompletionTerminal([]any{}),
		})
		require.NoError(t, err)
		require.Equal(t, text, record.Result)
		require.True(t, capture.truncated, "text recovery must not depend on retained raw diagnostics")
		require.False(t, capture.textTruncated)
	})
	for _, path := range []string{"capture", "http-intelligent", "ws-intelligent"} {
		t.Run("text_overflow/"+path, func(t *testing.T) {
			_, capture, _, err := replayEmptyCompletion(t, path, []map[string]any{
				emptyCompletionMessageDone(1, strings.Repeat("x", intelligentCaptureTextLimit+1)), emptyCompletionTerminal([]any{}),
			})
			require.Error(t, err)
			require.False(t, capture.upstreamComplete())
			if path == "capture" {
				require.EqualError(t, err, "test output exceeded capture limit")
			}
		})
		t.Run("item_count_overflow/"+path, func(t *testing.T) {
			events := make([]map[string]any, 0, intelligentCompletedOutputItemLimit+2)
			for i := 0; i <= intelligentCompletedOutputItemLimit; i++ {
				events = append(events, emptyCompletionMessageDone(i, "x"))
			}
			events = append(events, emptyCompletionTerminal([]any{}))
			_, capture, _, err := replayEmptyCompletion(t, path, events)
			require.Error(t, err)
			require.False(t, capture.upstreamComplete())
			if path == "capture" {
				require.EqualError(t, err, "test output exceeded capture limit")
			}
		})
	}
}

func TestIntelligentEmptyCompletionSanitizedReplayRecoversCompletedMessage(t *testing.T) {
	for _, path := range emptyCompletionReplayPaths {
		t.Run(path, func(t *testing.T) {
			record, capture, events, err := replayEmptyCompletion(t, path, emptyCompletionReplayFixture(t))
			require.NoError(t, err)
			require.Equal(t, "29个", record.Result)
			if capture != nil {
				require.True(t, capture.upstreamComplete())
			}
			var output strings.Builder
			contents, completions := 0, 0
			for _, event := range events {
				if event.Type == "content" {
					contents++
					output.WriteString(event.Text)
				}
				if event.Type == "test_complete" && event.Success {
					completions++
				}
			}
			if path != "capture" {
				require.Equal(t, "29个", output.String(), "text.done, part.done and item.done must not duplicate output")
				require.Equal(t, 1, completions)
				if strings.HasSuffix(path, "intelligent") {
					require.Equal(t, 1, contents)
				}
			}
		})
	}
}
