package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func accountOpenAIStreamEvent(t *testing.T, event map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(event)
	require.NoError(t, err)
	return "data: " + string(raw) + "\n\n"
}

func accountOpenAIStreamDelta(t *testing.T, text string) string {
	t.Helper()
	return accountOpenAIStreamEvent(t, map[string]any{"type": "response.output_text.delta", "delta": text})
}

func accountOpenAIStreamTerminal(t *testing.T, kind string, text *string) string {
	t.Helper()
	response := map[string]any{"status": "completed"}
	if text != nil {
		response["output"] = []any{map[string]any{
			"type": "message", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": *text}},
		}}
	}
	return accountOpenAIStreamEvent(t, map[string]any{"type": kind, "response": response})
}

func accountOpenAIStreamTestEvents(t *testing.T, raw string) []TestEvent {
	t.Helper()
	var events []TestEvent
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event TestEvent
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event))
		events = append(events, event)
	}
	return events
}

func runAccountOpenAIStream(t *testing.T, body io.Reader, intelligent bool) ([]TestEvent, *intelligentCapture, error) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx := context.Background()
	var capture *intelligentCapture
	if intelligent {
		capture = &intelligentCapture{}
		ctx = context.WithValue(ctx, intelligentRunKey{}, &intelligentRunContext{capture: capture})
		body = io.TeeReader(body, capture)
	}
	c.Request = httptest.NewRequest("POST", "/test", nil).WithContext(ctx)
	err := (&AccountTestService{}).processOpenAIStream(c, body)
	return accountOpenAIStreamTestEvents(t, w.Body.String()), capture, err
}

func TestAccountTestProcessOpenAIStreamFinalOnlyAtEOF(t *testing.T) {
	text := "<!doctype html><html><body><svg>鹈鹕</svg></body></html>"
	for _, kind := range []string{"response.completed", "response.done"} {
		for _, intelligent := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/live", true: "/intelligent"}[intelligent], func(t *testing.T) {
				stream := strings.TrimSuffix(accountOpenAIStreamTerminal(t, kind, &text), "\n\n")
				events, capture, err := runAccountOpenAIStream(t, strings.NewReader(stream), intelligent)
				require.NoError(t, err)
				require.Equal(t, []TestEvent{{Type: "content", Text: text}, {Type: "test_complete", Success: true}}, events)
				if intelligent {
					require.Equal(t, text, capture.outputText())
					require.True(t, capture.upstreamComplete())
				}
			})
		}
	}
}

func TestAccountTestProcessOpenAIStreamReconcilesFinalOutput(t *testing.T) {
	complete := "<svg><path d=\"M0 0\"/></svg>"
	partial := "<svg>"
	conflict := "partial-other-output"
	for _, test := range []struct {
		name        string
		deltas      []string
		final       *string
		intelligent bool
		want        []TestEvent
		wantError   string
	}{
		{name: "live missing suffix", deltas: []string{partial}, final: &complete, want: []TestEvent{{Type: "content", Text: partial}, {Type: "content", Text: strings.TrimPrefix(complete, partial)}, {Type: "test_complete", Success: true}}},
		{name: "live no duplicate full output", deltas: []string{partial, strings.TrimPrefix(complete, partial)}, final: &complete, want: []TestEvent{{Type: "content", Text: partial}, {Type: "content", Text: strings.TrimPrefix(complete, partial)}, {Type: "test_complete", Success: true}}},
		{name: "live conflict fails without replay", deltas: []string{conflict}, final: &complete, want: []TestEvent{{Type: "content", Text: conflict}}, wantError: "final output differs"},
		{name: "intelligent only authoritative final", deltas: []string{partial, strings.TrimPrefix(complete, partial)}, final: &complete, intelligent: true, want: []TestEvent{{Type: "content", Text: complete}, {Type: "test_complete", Success: true}}},
		{name: "intelligent final repairs conflicting deltas", deltas: []string{conflict}, final: &complete, intelligent: true, want: []TestEvent{{Type: "content", Text: complete}, {Type: "test_complete", Success: true}}},
		{name: "live absent final uses deltas", deltas: []string{complete}, want: []TestEvent{{Type: "content", Text: complete}, {Type: "test_complete", Success: true}}},
		{name: "intelligent absent final uses deltas", deltas: []string{partial, strings.TrimPrefix(complete, partial)}, intelligent: true, want: []TestEvent{{Type: "content", Text: complete}, {Type: "test_complete", Success: true}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stream strings.Builder
			for _, delta := range test.deltas {
				stream.WriteString(accountOpenAIStreamDelta(t, delta))
			}
			stream.WriteString(accountOpenAIStreamTerminal(t, "response.completed", test.final))
			events, _, err := runAccountOpenAIStream(t, strings.NewReader(stream.String()), test.intelligent)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
				require.Len(t, events, len(test.want)+1)
				require.Equal(t, "error", events[len(events)-1].Type)
				require.Equal(t, test.want, events[:len(events)-1])
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, events)
		})
	}
}

func TestAccountTestProcessOpenAIStreamExplicitEmptyFinalIsAuthoritative(t *testing.T) {
	terminal := accountOpenAIStreamEvent(t, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{}}})
	stream := accountOpenAIStreamDelta(t, "discarded draft") + terminal
	events, capture, err := runAccountOpenAIStream(t, strings.NewReader(stream), true)
	require.NoError(t, err)
	require.Equal(t, []TestEvent{{Type: "test_complete", Success: true}}, events)
	require.Empty(t, capture.outputText())
	require.True(t, capture.authoritativeText)
	events, _, err = runAccountOpenAIStream(t, strings.NewReader(stream), false)
	require.ErrorContains(t, err, "final output differs")
	require.Equal(t, "discarded draft", events[0].Text)
	require.Equal(t, "error", events[1].Type)
}

func TestAccountTestProcessOpenAIStreamFailurePreservesCaptureOnly(t *testing.T) {
	for _, test := range []struct {
		name     string
		terminal map[string]any
		wantErr  string
	}{
		{name: "failed", terminal: map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"message": "quota exhausted"}}}, wantErr: "quota exhausted"},
		{name: "incomplete", terminal: map[string]any{"type": "response.incomplete", "response": map[string]any{"incomplete_details": map[string]any{"reason": "max_output_tokens"}}}, wantErr: "max_output_tokens"},
		{name: "cancelled", terminal: map[string]any{"type": "response.cancelled"}, wantErr: "cancelled"},
		{name: "canceled", terminal: map[string]any{"type": "response.canceled"}, wantErr: "canceled"},
		{name: "error", terminal: map[string]any{"type": "error", "error": map[string]any{"message": "upstream refused"}}, wantErr: "upstream refused"},
		{name: "completed but incomplete", terminal: map[string]any{"type": "response.completed", "response": map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"}}}, wantErr: "max_output_tokens"},
		{name: "done but failed", terminal: map[string]any{"type": "response.done", "response": map[string]any{"status": "failed"}}, wantErr: "not completed successfully"},
		{name: "completed with unfinished item", terminal: map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "status": "in_progress"}}}}, wantErr: "not completed successfully"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := accountOpenAIStreamDelta(t, "partial SVG") + strings.TrimRight(accountOpenAIStreamEvent(t, test.terminal), "\n")
			events, capture, err := runAccountOpenAIStream(t, strings.NewReader(stream), true)
			require.ErrorContains(t, err, test.wantErr)
			require.Len(t, events, 1)
			require.Equal(t, "error", events[0].Type)
			require.Equal(t, "partial SVG", capture.outputText())
			require.False(t, capture.upstreamComplete())
		})
	}
}

func TestAccountTestProcessOpenAIStreamFailedFinalOnlyPreservesDiagnosticOutput(t *testing.T) {
	partial := "<html><svg><path"
	for _, kind := range []string{"response.incomplete", "response.failed", "response.cancelled", "response.canceled", "response.completed", "response.done"} {
		t.Run(kind, func(t *testing.T) {
			terminal := map[string]any{"type": kind, "response": map[string]any{
				"status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"},
				"output": []any{map[string]any{"type": "message", "status": "incomplete", "content": []any{map[string]any{"type": "output_text", "text": partial}}}},
			}}
			stream := strings.TrimRight(accountOpenAIStreamEvent(t, terminal), "\n")
			events, capture, err := runAccountOpenAIStream(t, strings.NewReader(stream), true)
			require.Error(t, err)
			require.Len(t, events, 1)
			require.Equal(t, "error", events[0].Type)
			require.False(t, capture.upstreamComplete())
			require.Equal(t, partial, capture.outputText(), "failed terminal output remains diagnostic, never a successful content event")
			record := &IntelligentTestRecord{ConfigSnapshot: &IntelligentTestConfig{}}
			require.Error(t, finalizeIntelligentTestRun(record, capture, nil, "", "", "", err))
			require.Equal(t, partial, record.Result)
		})
	}
}

type accountOpenAIStreamErrorReader struct{ err error }

func (r accountOpenAIStreamErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestAccountTestProcessOpenAIStreamMissingTerminalAndReadError(t *testing.T) {
	partial := accountOpenAIStreamDelta(t, "partial SVG")
	for _, test := range []struct {
		name    string
		body    io.Reader
		wantErr string
	}{
		{name: "EOF", body: strings.NewReader(strings.TrimRight(partial, "\n")), wantErr: "before response.completed"},
		{name: "DONE", body: strings.NewReader(partial + "data: [DONE]"), wantErr: "before response.completed"},
		{name: "read error", body: io.MultiReader(strings.NewReader(partial), accountOpenAIStreamErrorReader{err: errors.New("broken connection")}), wantErr: "Stream read error: broken connection"},
	} {
		t.Run(test.name, func(t *testing.T) {
			events, capture, err := runAccountOpenAIStream(t, test.body, true)
			require.ErrorContains(t, err, test.wantErr)
			require.Len(t, events, 1)
			require.Equal(t, "error", events[0].Type)
			require.Equal(t, "partial SVG", capture.outputText())
			require.False(t, capture.upstreamComplete())
		})
	}
}

func TestAccountTestProcessOpenAIStreamTextLimit(t *testing.T) {
	limit := strings.Repeat("x", intelligentCaptureTextLimit)
	tooLarge := limit + "x"
	for _, test := range []struct {
		name   string
		stream string
		ok     bool
	}{
		{name: "final at limit", stream: accountOpenAIStreamTerminal(t, "response.completed", &limit), ok: true},
		{name: "delta and final counted once", stream: accountOpenAIStreamDelta(t, limit) + accountOpenAIStreamTerminal(t, "response.completed", &limit), ok: true},
		{name: "final over limit", stream: accountOpenAIStreamTerminal(t, "response.completed", &tooLarge)},
		{name: "delta over limit", stream: accountOpenAIStreamDelta(t, tooLarge)},
		{name: "cumulative deltas over limit", stream: accountOpenAIStreamDelta(t, limit) + accountOpenAIStreamDelta(t, "x")},
	} {
		t.Run(test.name, func(t *testing.T) {
			events, _, err := runAccountOpenAIStream(t, strings.NewReader(test.stream), true)
			if test.ok {
				require.NoError(t, err)
				require.Len(t, events, 2)
				require.Equal(t, limit, events[0].Text)
				require.Equal(t, "test_complete", events[1].Type)
			} else {
				require.ErrorContains(t, err, "too large")
				require.Len(t, events, 1)
				require.Equal(t, "error", events[0].Type)
			}
		})
	}
}

type accountOpenAIStreamCheckReader struct {
	reader io.Reader
	check  func()
}

func (r *accountOpenAIStreamCheckReader) Read(p []byte) (int, error) {
	if r.check != nil {
		r.check()
		r.check = nil
	}
	return r.reader.Read(p)
}

func TestAccountTestProcessOpenAIStreamLiveDeltasRemainImmediate(t *testing.T) {
	for _, intelligent := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "intelligent"}[intelligent], func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			ctx := context.Background()
			if intelligent {
				ctx = context.WithValue(ctx, intelligentRunKey{}, &intelligentRunContext{})
			}
			c.Request = httptest.NewRequest("POST", "/test", nil).WithContext(ctx)
			final := "<svg>complete</svg>"
			checked := false
			last := &accountOpenAIStreamCheckReader{
				reader: strings.NewReader(accountOpenAIStreamTerminal(t, "response.completed", &final)),
				check: func() {
					checked = true
					events := accountOpenAIStreamTestEvents(t, w.Body.String())
					if intelligent {
						require.Empty(t, events)
					} else {
						require.Equal(t, []TestEvent{{Type: "content", Text: "<svg>"}}, events)
					}
				},
			}
			body := io.MultiReader(strings.NewReader(accountOpenAIStreamDelta(t, "<svg>")), last)
			require.NoError(t, (&AccountTestService{}).processOpenAIStream(c, body))
			require.True(t, checked)
		})
	}
}
