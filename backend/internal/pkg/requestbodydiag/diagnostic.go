// Package requestbodydiag records bounded upload diagnostics without payloads.
package requestbodydiag

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const Component = "http.request_body"

// A single process-wide budget bounds writes even for rotating keys/IPs.
// There is no per-user cardinality, timer, request-body retention or goroutine.
var failureBudget = &eventBudget{limit: 60, window: time.Minute}

type eventBudget struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	started time.Time
	emitted int
	dropped uint64
}

func (b *eventBudget) allow(now time.Time) (bool, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var summary uint64
	if b.started.IsZero() || now.Before(b.started) || now.Sub(b.started) >= b.window {
		summary = b.dropped
		b.started, b.emitted, b.dropped = now, 0, 0
	}
	if b.limit <= 0 || b.window <= 0 || b.emitted >= b.limit {
		b.dropped++
		return false, summary
	}
	b.emitted++
	return true, summary
}

// LogFailure writes one whitelisted system-log event independently of the
// console log level and access-log persistence setting. The normal Warn entry
// is explicitly excluded from the sink so warn/debug logging cannot duplicate
// the persisted event. An absent/stopped sink retains its existing no-op policy.
func LogFailure(reqLog *zap.Logger, req *http.Request, err error) {
	if err == nil {
		return
	}
	allowed, dropped := failureBudget.allow(time.Now())
	if dropped > 0 {
		logger.WriteSinkEvent("warn", Component, "request body diagnostics suppressed", map[string]any{
			"dropped_count":  dropped,
			"window_seconds": int64(time.Minute / time.Second),
		})
	}
	if !allowed {
		return
	}
	fields := map[string]any{
		"error_kind":       httputil.RequestBodyReadErrorKind(err),
		"content_encoding": "identity",
		"content_length":   int64(-1),
	}
	if req != nil {
		fields["content_encoding"] = httputil.RequestContentEncodingCategory(req.Header.Get("Content-Encoding"))
		fields["content_length"] = req.ContentLength
		// Correlation comes from the normalized request context, never raw headers.
		if id, ok := req.Context().Value(ctxkey.RequestID).(string); ok && validCorrelationID(id) {
			fields["request_id"] = id
		}
		if id, ok := req.Context().Value(ctxkey.ClientRequestID).(string); ok && validCorrelationID(id) {
			fields["client_request_id"] = id
		}
		if id, ok := req.Context().Value(ctxkey.UserID).(int64); ok && id > 0 {
			fields["user_id"] = id
		}
		state := "active"
		switch {
		case errors.Is(req.Context().Err(), context.Canceled):
			state = "canceled"
		case errors.Is(req.Context().Err(), context.DeadlineExceeded):
			state = "deadline_exceeded"
		}
		fields["context_state"] = state
	}
	if measured, ok := httputil.RequestBodyReadDiagnostics(err); ok {
		fields["received_bytes"] = measured.ReceivedBytes
		fields["read_elapsed_ms"] = measured.ReadElapsedMS
		fields["read_idle_ms"] = measured.ReadIdleMS
	}
	logger.WriteSinkEvent("warn", Component, "read request body failed", fields)
	if reqLog == nil {
		reqLog = logger.L()
		if req != nil {
			reqLog = logger.FromContext(req.Context())
		}
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	logFields := make([]zap.Field, 0, len(keys)+2)
	for _, key := range keys {
		logFields = append(logFields, zap.Any(key, fields[key]))
	}
	logFields = append(logFields, zap.String("component", Component), zap.Bool(logger.OpsSystemLogSkipField, true))
	reqLog.Warn("read request body failed", logFields...)
}

func validCorrelationID(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for i := range value {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':' {
			continue
		}
		return false
	}
	return true
}
