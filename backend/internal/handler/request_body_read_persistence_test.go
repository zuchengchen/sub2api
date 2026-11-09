//go:build unit

package handler

import (
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

type requestBodyPersistenceSink struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (s *requestBodyPersistenceSink) WriteLogEvent(event *logger.LogEvent) {
	if event.Message != "read request body failed" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if skip, _ := event.Fields[logger.OpsSystemLogSkipField].(bool); !skip {
		s.events = append(s.events, event)
	}
}

func (s *requestBodyPersistenceSink) snapshot() []*logger.LogEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*logger.LogEvent(nil), s.events...)
}

// This test compiles against the unmodified baseline too: before the change it
// fails with zero sink events at level=error, after the change it observes one.
func TestRequestBodyReadFailurePersistsWithErrorLogLevel(t *testing.T) {
	if err := logger.Init(logger.InitOptions{Level: "error", Format: "json", StacktraceLevel: "none", Output: logger.OutputOptions{ToStdout: true}}); err != nil {
		t.Fatal(err)
	}
	sink := &requestBodyPersistenceSink{}
	logger.SetSink(sink)
	t.Cleanup(func() { logger.SetSink(nil) })
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	logRequestBodyReadFailure(nil, req, io.ErrUnexpectedEOF)
	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("upload reason must persist with production log level: got %d events, want 1", len(events))
	}
	if events[0].Fields["error_kind"] != "truncated_body" {
		t.Fatalf("wrong body failure kind: %+v", events[0].Fields)
	}
}
