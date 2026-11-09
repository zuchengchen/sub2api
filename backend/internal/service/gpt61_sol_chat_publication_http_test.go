package service_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Only the first response gets this test gate. It flushes the real DONE bytes
// before waiting for real client cancellation, exposing the otherwise narrow
// DONE -> finish race without mocking a request context or cache publication.
type gpt61PublicationHTTPGate struct {
	http.ResponseWriter
	ctx      context.Context
	mode     string
	done     bool
	observed chan bool
}

func (w *gpt61PublicationHTTPGate) canceled() bool {
	select {
	case <-w.ctx.Done():
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}
func (w *gpt61PublicationHTTPGate) Write(p []byte) (int, error) {
	isDone := bytes.Contains(p, []byte("[DONE]"))
	if isDone && w.mode == "before_done_close" {
		w.observed <- w.canceled()
	}
	if isDone && w.mode == "short_done" {
		return 0, io.ErrShortWrite
	}
	n, err := w.ResponseWriter.Write(p)
	if isDone && n == len(p) && err == nil {
		w.done = true
	}
	return n, err
}
func (w *gpt61PublicationHTTPGate) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *gpt61PublicationHTTPGate) Flush() {
	w.ResponseWriter.(http.Flusher).Flush()
	if w.done && (w.mode == "after_done_close" || w.mode == "after_done_timeout") {
		w.done = false
		w.observed <- w.canceled()
		if w.mode == "after_done_timeout" {
			<-time.After(150 * time.Millisecond)
		}
	}
}

func TestGPT61SolChatCachePublicationImmediateDONECloseHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, model, mode, gate   string
		assistant, ordinary, want bool
	}{
		{"ordinary_target_tools", "gpt-6.1-sol", "replay", "after_done_close", false, true, true},
		{"target_assistant", "gpt-6.1-sol", "replay", "after_done_close", true, false, true},
		{"legacy_other_model", "gpt-6-astra", "replay", "after_done_close", false, false, false},
		{"observe_unchanged", "gpt-6.1-sol", "observe", "after_done_close", false, true, false},
		{"canceled_before_done", "gpt-6.1-sol", "replay", "before_done_close", false, true, false},
		{"short_done_write", "gpt-6.1-sol", "replay", "short_done", false, true, false},
		{"flush_consumes_original_budget", "gpt-6.1-sol", "replay", "after_done_timeout", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var serverHandler http.Handler
			var serverURL string
			var setHandler func(http.Handler)
			var store *nativeTrafficStore
			var rows func() []nativeTrafficRow
			var forwardOK func()
			if tc.ordinary {
				policy := gpt61ScopeAllUsersPolicy()
				policy.config.Mode = tc.mode
				h := newGPT61ScopeHTTPHarness(t, policy, nil, "")
				serverHandler, serverURL, store = h.server.Config.Handler, h.server.URL, h.store
				setHandler = func(v http.Handler) { h.server.Config.Handler = v }
				rows = func() []nativeTrafficRow {
					h.mu.Lock()
					defer h.mu.Unlock()
					return append([]nativeTrafficRow(nil), h.rows...)
				}
				forwardOK = func() { h.assertNoForwardErrors(t) }
			} else {
				h := newNativeTrafficHarness(t, tc.mode)
				h.modelOverride = tc.model
				if tc.assistant {
					h.outputOverrides = map[int][]json.RawMessage{1: nativeTrafficAssistantOutput(1, true), 2: nativeTrafficAssistantOutput(2, true)}
				}
				serverHandler, serverURL, store = h.server.Config.Handler, h.server.URL, h.store
				setHandler = func(v http.Handler) { h.server.Config.Handler = v }
				rows = func() []nativeTrafficRow {
					h.mu.Lock()
					defer h.mu.Unlock()
					return append([]nativeTrafficRow(nil), h.rows...)
				}
				forwardOK = func() {
					h.mu.Lock()
					defer h.mu.Unlock()
					require.Empty(t, h.errors)
					require.NotEmpty(t, h.results)
					require.NotNil(t, h.results[0])
				}
			}
			observed := make(chan bool, 1)
			finished := make(chan bool, 1)
			var first sync.Once
			setHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				controlled := false
				first.Do(func() { controlled = true })
				if controlled {
					w = &gpt61PublicationHTTPGate{ResponseWriter: w, ctx: r.Context(), mode: tc.gate, observed: observed}
				}
				serverHandler.ServeHTTP(w, r)
				if controlled {
					finished <- r.Context().Err() != nil
				}
			}))
			transport := &http.Transport{DisableKeepAlives: true}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			tools := []any{}
			for _, name := range []string{"lookup_temperature", "lookup_humidity"} {
				tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name, "description": "Read synthetic local fixture data.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"location": map[string]any{"type": "string"}}, "required": []string{"location"}}}})
			}
			request := func(history []json.RawMessage) *http.Request {
				body := nativeTrafficJSON(map[string]any{"model": tc.model, "messages": history, "stream": true, "stream_options": map[string]any{"include_usage": true}, "tools": tools, "reasoning_effort": "high", "parallel_tool_calls": true})
				r, err := http.NewRequest(http.MethodPost, serverURL+"/v1/chat/completions", bytes.NewReader(body))
				require.NoError(t, err)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("session-id", "synthetic-completion-boundary")
				if tc.ordinary {
					r.Header.Set("X-Fixture-User", "101")
					r.Header.Set("X-Fixture-Key", "501")
				}
				return r
			}
			history := nativeTrafficInitialHistory()
			response, err := client.Do(request(history))
			require.NoError(t, err)
			require.Equal(t, 200, response.StatusCode)
			reader := bufio.NewReader(response.Body)
			var received bytes.Buffer
			gotDone := false
			for {
				line, readErr := reader.ReadBytes('\n')
				received.Write(line)
				if readErr != nil {
					require.Equal(t, "short_done", tc.gate)
					break
				}
				completeLine := bytes.Equal(bytes.TrimSpace(line), []byte("data: [DONE]"))
				beforeDone := tc.gate == "before_done_close" && (bytes.Contains(line, []byte(`"finish_reason":"tool_calls"`)) || bytes.Contains(line, []byte(`"finish_reason":"stop"`)))
				if completeLine || beforeDone {
					blank, err := reader.ReadBytes('\n')
					require.NoError(t, err)
					received.Write(blank)
					gotDone = completeLine
					break
				}
			}
			require.NoError(t, response.Body.Close())
			if tc.gate != "short_done" {
				select {
				case canceled := <-observed:
					require.True(t, canceled, "the real first request must be canceled at the controlled boundary")
				case <-time.After(3 * time.Second):
					t.Fatal("completion gate was not reached")
				}
			}
			if tc.want {
				require.True(t, gotDone)
				assistant, err := nativeTrafficDecodeChatSSE(received.Bytes())
				require.NoError(t, err)
				nextHistory := nativeTrafficContinue(history, assistant)
				if tc.assistant {
					nextHistory = append(nextHistory, json.RawMessage(`{"role":"user","content":"Continue after the completed assistant."}`))
				}
				next, err := client.Do(request(nextHistory))
				require.NoError(t, err)
				nextRaw, err := io.ReadAll(next.Body)
				require.NoError(t, err)
				require.NoError(t, next.Body.Close())
				_, err = nativeTrafficDecodeChatSSE(nextRaw)
				require.NoError(t, err)
				captured := rows()
				require.Len(t, captured, 2)
				if tc.assistant {
					require.Zero(t, nativeTrafficCompareOutputBlock(captured[1].Body, captured[0].Output, true, false), "a completed ordinary assistant must survive immediate DONE close")
				} else {
					require.Zero(t, nativeTrafficComparisonExit(captured[1].Body, captured[0].Output), "the next real upstream request must retain the full preceding native block")
				}
			}
			select {
			case canceled := <-finished:
				if tc.gate != "short_done" {
					require.True(t, canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("first request did not finish")
			}
			store.mu.Lock()
			count := len(store.values)
			store.mu.Unlock()
			if tc.want {
				require.GreaterOrEqual(t, count, 1)
			} else {
				require.Zero(t, count, "an unconfirmed or out-of-scope completion must not publish")
			}
			if tc.gate != "short_done" && tc.gate != "before_done_close" {
				forwardOK()
			}
		})
	}
}

var _ service.NativeReasoningReplayStore = (*nativeTrafficStore)(nil)
