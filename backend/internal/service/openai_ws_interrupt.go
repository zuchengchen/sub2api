package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var errOpenAIWSInterruptProtocol = errors.New("invalid websocket interruption lifecycle")

// A cancel is reusable only after both its exact acknowledgement and response
// terminal. Fixed Codex bfdbf01 responses_websocket.rs:812-890,958-975 permits
// either arrival order, but never treats an unknown/failed acknowledgement as
// success. All fields belong to one leased response, never a global session.
type openAIWSInterruptState struct {
	responseID                                      string
	requested, accepted, terminal, reusableTerminal bool
	failure                                         error
	ackValid                                        bool
}

func (s *openAIWSInterruptState) request(raw []byte) error {
	if s == nil || s.requested || s.responseID == "" || !json.Valid(raw) || !utf8.Valid(raw) || hasDuplicateJSONObjectKeys(gjson.ParseBytes(raw)) {
		return errOpenAIWSInterruptProtocol
	}
	var request struct {
		Type       string `json:"type"`
		ResponseID string `json:"response_id"`
		Mode       string `json:"mode"`
	}
	if json.Unmarshal(raw, &request) != nil || request.Type != "response.interrupt" || request.ResponseID != s.responseID || request.Mode != "discard_partial_items" {
		return errOpenAIWSInterruptProtocol
	}
	s.requested = true
	return nil
}

func (s *openAIWSInterruptState) automaticRequest() []byte {
	if s == nil || s.requested || s.responseID == "" {
		return nil
	}
	raw, _ := json.Marshal(struct {
		Type       string `json:"type"`
		ResponseID string `json:"response_id"`
		Mode       string `json:"mode"`
	}{"response.interrupt", s.responseID, "discard_partial_items"})
	s.requested = true
	return raw
}

func (s *openAIWSInterruptState) observe(raw []byte, eventType, responseID string) (bool, error) {
	ack := eventType == "response.interrupt.accepted" || eventType == "response.interrupt.failed"
	if ack {
		s.ackValid = false
		var value struct {
			Type       string `json:"type"`
			ResponseID string `json:"response_id"`
		}
		if !s.requested || s.accepted || s.failure != nil || !json.Valid(raw) || !utf8.Valid(raw) || hasDuplicateJSONObjectKeys(gjson.ParseBytes(raw)) || json.Unmarshal(raw, &value) != nil || value.ResponseID != s.responseID || value.Type != eventType {
			s.failure = errOpenAIWSInterruptProtocol
			return true, s.failure
		}
		s.ackValid = true
		if eventType == "response.interrupt.failed" {
			s.failure = errOpenAIWSInterruptProtocol
			return true, s.failure
		}
		s.accepted = true
		return true, nil
	}
	if responseID != "" && s.responseID == "" {
		s.responseID = responseID
	}
	if isOpenAIWSTerminalEvent(eventType) {
		// gjson can expose fields from a truncated document; an interruption
		// must not be confirmed by such a terminal or duplicate identity keys.
		if s.requested && (!json.Valid(raw) || !utf8.Valid(raw) || hasDuplicateJSONObjectKeys(gjson.ParseBytes(raw)) || hasDuplicateJSONObjectKeys(gjson.GetBytes(raw, "response")) || responseID != s.responseID) {
			s.failure = errOpenAIWSInterruptProtocol
			return false, s.failure
		}
		s.terminal = true
		s.reusableTerminal = eventType == "response.completed" || eventType == "response.done" || (eventType == "response.incomplete" && gjson.GetBytes(raw, "response.incomplete_details.reason").String() == "interrupted")
	}
	return false, nil
}

func (s *openAIWSInterruptState) ready() bool {
	return s.terminal && (!s.requested || s.accepted || s.failure != nil)
}
func (s *openAIWSInterruptState) reusable() bool {
	return s.failure == nil && (!s.requested || (s.accepted && s.terminal && s.reusableTerminal))
}

const openAIWSInterruptClientReaderKey = "openai_ws_interrupt_client_reader"

// The downstream reader belongs to the accepted socket, not to an upstream
// account attempt. Keeping it in this connection's gin context allows pre-output
// failover to reuse the same reader without closing the client or racing reads.
// Its one-slot queue bounds lookahead; it is terminated by actual socket/request
// closure. It never reads credentials or touches another connection's state.
type openAIWSInterruptClientReader struct {
	conn   *coderws.Conn
	frames chan openAIWSClientReadResult
	closed context.Context
	done   chan struct{}
	queued *openAIWSClientReadResult // owned only by the serial proxy attempt
	stop   context.CancelCauseFunc
}

func openAIWSExistingInterruptClientReader(c *gin.Context, conn *coderws.Conn) *openAIWSInterruptClientReader {
	if c != nil {
		if value, ok := c.Get(openAIWSInterruptClientReaderKey); ok {
			if reader, ok := value.(*openAIWSInterruptClientReader); ok && reader.conn == conn {
				return reader
			}
		}
	}
	return nil
}

func openAIWSGetInterruptClientReader(c *gin.Context, conn *coderws.Conn, lifecycle context.Context) *openAIWSInterruptClientReader {
	if c != nil {
		if value, ok := c.Get(openAIWSInterruptClientReaderKey); ok {
			if reader, ok := value.(*openAIWSInterruptClientReader); ok && reader.conn == conn {
				return reader
			}
		}
	}
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	closed, cancel := context.WithCancelCause(context.Background())
	readerCtx, stopReader := context.WithCancelCause(lifecycle)
	reader := &openAIWSInterruptClientReader{conn: conn, frames: make(chan openAIWSClientReadResult, 1), closed: closed, done: make(chan struct{}), stop: stopReader}
	if c != nil {
		c.Set(openAIWSInterruptClientReaderKey, reader)
	}
	go func() {
		defer close(reader.done)
		defer close(reader.frames)
		stop := context.AfterFunc(readerCtx, func() { _ = conn.CloseNow() })
		defer stop()
		for {
			kind, payload, err := conn.Read(context.Background())
			frame := openAIWSClientReadResult{messageType: kind, payload: payload, err: err}
			if err != nil {
				cancel(err)
			}
			if err != nil {
				// A queued lookahead frame must not prevent a closed socket's
				// reader from terminating. The closed signal already preserves
				// the error for the current/next attempt.
				select {
				case reader.frames <- frame:
				default:
				}
				return
			}
			select {
			case reader.frames <- frame:
			case <-readerCtx.Done():
				cancel(context.Cause(readerCtx))
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return reader
}

func (r *openAIWSInterruptClientReader) controls() <-chan openAIWSClientReadResult {
	if r == nil || r.queued != nil {
		return nil
	}
	return r.frames
}

func (r *openAIWSInterruptClientReader) next(ctx context.Context, timeout time.Duration) ([]byte, error) {
	_, body, err := r.nextFrame(ctx, timeout, nil, nil)
	return body, err
}

func (r *openAIWSInterruptClientReader) nextFrame(ctx context.Context, timeout time.Duration, start <-chan struct{}, active func() bool) (coderws.MessageType, []byte, error) {
	if r.queued != nil {
		frame := *r.queued
		r.queued = nil
		return frame.messageType, frame.payload, frame.err
	}
	var timer *time.Timer
	var timeoutCh <-chan time.Time
	startTimeout := func() {
		if timeout <= 0 || (active != nil && !active()) {
			return
		}
		if timer == nil {
			timer = time.NewTimer(timeout)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		}
		timeoutCh = timer.C
	}
	startTimeout()
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	closeAndJoin := func(status coderws.StatusCode, reason string, cause error) (coderws.MessageType, []byte, error) {
		_ = r.conn.Close(status, reason)
		_ = r.conn.CloseNow()
		r.stop(cause)
		<-r.done
		return 0, nil, NewOpenAIWSClientCloseError(status, reason, cause)
	}
	for {
		select {
		case frame, ok := <-r.frames:
			if !ok {
				return 0, nil, context.Cause(r.closed)
			}
			return frame.messageType, frame.payload, frame.err
		case <-start:
			startTimeout()
		case <-timeoutCh:
			return closeAndJoin(coderws.StatusNormalClosure, "websocket idle timeout", context.DeadlineExceeded)
		case <-ctx.Done():
			cause := context.Cause(ctx)
			if errors.Is(cause, ErrOpenAIWSIngressLeaseLost) {
				return closeAndJoin(coderws.StatusTryAgainLater, "websocket ingress capacity lease lost; please reconnect", cause)
			}
			return closeAndJoin(coderws.StatusGoingAway, "websocket request canceled", cause)
		}
	}
}

type openAIWSInterruptReadResult struct {
	body []byte
	err  error
}

// Real pooled sockets already have a network read loop. Select its queue directly
// so a cancellation signal does not abort the socket before interrupt can be
// sent. Direct transports use at most one joined read goroutine per outstanding
// read; no second read or lease release may race it.
type openAIWSInterruptReader struct {
	lease    *openAIWSConnLease
	pending  chan openAIWSInterruptReadResult
	cancel   context.CancelFunc
	deadline time.Time
}

func (r *openAIWSInterruptReader) close() {
	if r.pending != nil {
		r.cancel()
		r.lease.MarkBroken()
		<-r.pending
		r.pending = nil
	}
}

func (r *openAIWSInterruptReader) read(ctx context.Context, timeout time.Duration, controls <-chan openAIWSClientReadResult) ([]byte, *openAIWSClientReadResult, error) {
	conn, err := r.lease.activeConn()
	if err != nil {
		return nil, nil, err
	}
	if r.deadline.IsZero() {
		r.deadline = time.Now().Add(timeout)
	}
	remaining := time.Until(r.deadline)
	if timeout <= 0 {
		return nil, nil, errOpenAIWSInterruptProtocol
	}
	if remaining <= 0 {
		return nil, nil, context.DeadlineExceeded
	}
	if conn.readerLoopResults == nil && r.pending == nil {
		readCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r.cancel = cancel
		r.pending = make(chan openAIWSInterruptReadResult, 1)
		pending := r.pending
		go func() {
			body, err := r.lease.ReadMessageWithContextTimeout(readCtx, remaining)
			pending <- openAIWSInterruptReadResult{body, err}
		}()
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case frame, ok := <-controls:
		if !ok {
			return nil, &openAIWSClientReadResult{err: context.Canceled}, nil
		}
		return nil, &frame, nil
	case body, ok := <-conn.readerLoopResults:
		r.deadline = time.Time{}
		if !ok {
			return nil, nil, conn.readerLoopError()
		}
		conn.touch()
		return body, nil, nil
	case result := <-r.pending:
		r.cancel()
		r.pending = nil
		r.deadline = time.Time{}
		return result.body, nil, result.err
	case <-ctx.Done():
		// A capacity lease can be lost immediately after an already-complete
		// read. Join that read and deliver its confirmed frame before emitting
		// the retry close, exactly as the former synchronous read did.
		if errors.Is(context.Cause(ctx), ErrOpenAIWSIngressLeaseLost) {
			if r.pending != nil {
				r.cancel()
				result := <-r.pending
				r.pending = nil
				r.deadline = time.Time{}
				if result.err == nil {
					return result.body, nil, nil
				}
			} else {
				select {
				case body, ok := <-conn.readerLoopResults:
					if ok {
						r.deadline = time.Time{}
						conn.touch()
						return body, nil, nil
					}
				default:
				}
			}
		}
		return nil, nil, ctx.Err()
	case <-timer.C:
		return nil, nil, context.DeadlineExceeded
	}
}
