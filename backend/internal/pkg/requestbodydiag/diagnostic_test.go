package requestbodydiag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type observedSink struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (s *observedSink) WriteLogEvent(event *logger.LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *observedSink) snapshot() []*logger.LogEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*logger.LogEvent(nil), s.events...)
}

func installTestLogger(t *testing.T, level string) *observedSink {
	t.Helper()
	if err := logger.Init(logger.InitOptions{Level: level, Format: "json", StacktraceLevel: "none", Output: logger.OutputOptions{ToStdout: true}}); err != nil {
		t.Fatal(err)
	}
	sink := &observedSink{}
	logger.SetSink(sink)
	prior := failureBudget
	failureBudget = &eventBudget{limit: 60, window: time.Minute}
	t.Cleanup(func() { logger.SetSink(nil); failureBudget = prior })
	return sink
}

func TestFailurePersistsAtErrorLevelWithoutPayloadOrRawMetadata(t *testing.T) {
	sink := installTestLogger(t, "error")
	marker := "sensitive-marker"
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/"+marker+"?token="+marker, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+marker)
	req.Header.Set("Content-Encoding", marker)
	req.Header.Set("User-Agent", marker)
	req.ContentLength = -1
	req.Body = io.NopCloser(io.MultiReader(strings.NewReader(marker), iotest.ErrReader(fmt.Errorf("%s: %w", marker, io.ErrUnexpectedEOF))))
	ctx := context.WithValue(req.Context(), ctxkey.RequestID, "76a6df82-f516-4fdf-92e7-623e8ce92191")
	ctx = context.WithValue(ctx, ctxkey.UserID, int64(417))
	req = req.WithContext(ctx)
	_, readErr := httputil.ReadRequestBodyWithPrealloc(req)
	LogFailure(nil, req, readErr)
	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("wanted one persisted diagnostic at error level, got %d", len(events))
	}
	event := events[0]
	if event.Component != Component || event.Level != "warn" || event.Fields["error_kind"] != "truncated_body" || event.Fields["content_encoding"] != "other" || event.Fields["received_bytes"] != int64(len(marker)) || event.Fields["content_length"] != int64(-1) || event.Fields["user_id"] != int64(417) {
		t.Fatalf("incorrect diagnostic fields: %+v", event)
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), marker) || strings.Contains(string(raw), "example.invalid") || strings.Contains(string(raw), "Bearer") {
		t.Fatalf("sensitive input leaked: %s", raw)
	}
}

func TestWarningOutputDoesNotDuplicateTheSystemLog(t *testing.T) {
	sink := installTestLogger(t, "warn")
	LogFailure(nil, nil, io.ErrUnexpectedEOF)
	events := sink.snapshot()
	if len(events) != 2 {
		t.Fatalf("wanted direct sink plus skipped console event, got %d", len(events))
	}
	persistable := 0
	for _, event := range events {
		if skip, _ := event.Fields[logger.OpsSystemLogSkipField].(bool); !skip {
			persistable++
		}
		if _, fabricated := event.Fields["received_bytes"]; fabricated {
			t.Fatal("invented byte count for an unmeasured reader")
		}
	}
	if persistable != 1 {
		t.Fatalf("wanted exactly one non-skipped event, got %d", persistable)
	}
}

func TestNoFailureProducesNoDiagnostic(t *testing.T) {
	sink := installTestLogger(t, "error")
	LogFailure(nil, nil, nil)
	events := sink.snapshot()
	if len(events) != 0 || failureBudget.emitted != 0 {
		t.Fatal("success used diagnostic budget")
	}
	logger.SetSink(nil)
	LogFailure(nil, nil, io.ErrUnexpectedEOF) // An unavailable sink must be safe.
}

func TestDiagnosticBudgetIsGlobalConcurrentAndReportsSuppression(t *testing.T) {
	budget := &eventBudget{limit: 10, window: time.Minute}
	start := time.Unix(1000, 0)
	var mu sync.Mutex
	allowed := 0
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, summary := budget.allow(start)
			if summary != 0 {
				t.Error("unexpected summary within a window")
			}
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Fatalf("global budget allowed %d requests", allowed)
	}
	ok, summary := budget.allow(start.Add(time.Minute))
	if !ok || summary != 90 {
		t.Fatalf("new-window summary=%d allowed=%v", summary, ok)
	}
	_, summary = budget.allow(start.Add(time.Minute))
	if summary != 0 {
		t.Fatal("suppression was reported twice")
	}
}

func TestDiagnosticCorrelationIDIsBounded(t *testing.T) {
	for _, id := range []string{"", strings.Repeat("x", 65), "line\nbreak", "token=private", "/path?query", "中文"} {
		if validCorrelationID(id) {
			t.Fatalf("accepted non-correlation metadata %q", id)
		}
	}
	if !validCorrelationID("request-1.client_2:3") {
		t.Fatal("rejected bounded correlation ID")
	}
}
