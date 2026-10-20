package service

import (
	"context"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const openAIForwardTimingKey = "openai_forward_timing"

// One trace belongs to one HTTP forwarding attempt. Callbacks retain only this
// immutable snapshot, never Gin's pooled context or a mutable account pointer.
type openAIForwardTiming struct {
	started  time.Time
	log      *zap.Logger
	attempt  int
	semantic atomic.Bool
	flushed  atomic.Bool
}

func openAIForwardTimingFromContext(c *gin.Context) *openAIForwardTiming {
	if c == nil {
		return nil
	}
	value, _ := c.Get(openAIForwardTimingKey)
	timing, _ := value.(*openAIForwardTiming)
	return timing
}

func newOpenAIForwardTiming(c *gin.Context, request *http.Request, account *Account) *openAIForwardTiming {
	started := time.Now()
	ctx := request.Context()
	if c != nil && c.Request != nil {
		ctx = c.Request.Context()
	}
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	var accountID int64
	if account != nil {
		accountID = account.ID
	}
	attempt := 1
	if previous := openAIForwardTimingFromContext(c); previous != nil {
		attempt = previous.attempt + 1
	}
	timing := &openAIForwardTiming{
		started: started,
		attempt: attempt,
		log: logger.FromContext(ctx).With(
			zap.String("component", "service.openai_gateway"),
			zap.String("request_id", requestID),
			zap.Int64("account_id", accountID),
			zap.Int("attempt", attempt),
		),
	}
	if c != nil {
		c.Set(openAIForwardTimingKey, timing)
	}
	timing.record("start")
	return timing
}

func (t *openAIForwardTiming) context(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GetConn: func(string) {
			t.record("get_conn")
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.record("got_conn", zap.Bool("reused", info.Reused))
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			// An error can contain proxy credentials or URLs; only record success.
			t.record("wrote_request", zap.Bool("success", info.Err == nil))
		},
		GotFirstResponseByte: func() {
			t.record("first_response_byte")
		},
	})
}

func (t *openAIForwardTiming) record(stage string, fields ...zap.Field) {
	fields = append(fields, zap.String("stage", stage), zap.Int64("elapsed_ms", time.Since(t.started).Milliseconds()))
	t.log.Info("openai.forward_timing", fields...)
}

func (t *openAIForwardTiming) markResponse(response *http.Response, err error) {
	if response != nil {
		t.record("response_headers", zap.Int("status", response.StatusCode))
	}
	if err != nil {
		// Deliberately omit the error text: transport errors can embed secrets.
		t.record("upstream_error")
	}
}

// MarkSemantic excludes transport comments and protocol-only SSE events.
// The atomic fast paths do not allocate or log after the first marker.
func (t *openAIForwardTiming) MarkSemantic() {
	if t != nil && !t.semantic.Load() && t.semantic.CompareAndSwap(false, true) {
		t.record("first_semantic")
	}
}

// MarkFlush is called after Flush returns for semantic output, not heartbeats.
// This measures the local writer boundary, not remote-client receipt.
func (t *openAIForwardTiming) MarkFlush() {
	if t != nil && !t.flushed.Load() && t.flushed.CompareAndSwap(false, true) {
		t.record("first_downstream_flush")
	}
}
