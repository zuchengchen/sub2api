package httputil

import (
	"errors"
	"time"
)

// BodyReadDiagnostics describes the wire body read, excluding HTTP headers,
// chunk framing and decompression. It never retains request content.
type BodyReadDiagnostics struct {
	ReceivedBytes int64
	ReadElapsedMS int64
	ReadIdleMS    int64
}

type requestBodyReadDiagnosticError struct {
	cause       error
	diagnostics BodyReadDiagnostics
}

func (e *requestBodyReadDiagnosticError) Error() string { return e.cause.Error() }
func (e *requestBodyReadDiagnosticError) Unwrap() error { return e.cause }

// RequestBodyReadDiagnostics returns measured values only. Errors from other
// readers have no measurements; callers must not invent zero-byte evidence.
func RequestBodyReadDiagnostics(err error) (BodyReadDiagnostics, bool) {
	var measured *requestBodyReadDiagnosticError
	if errors.As(err, &measured) && measured != nil {
		return measured.diagnostics, true
	}
	return BodyReadDiagnostics{}, false
}

type requestBodyReadProgress struct {
	started       time.Time
	lastProgress  time.Time
	receivedBytes int64
}

func (p *requestBodyReadProgress) wrapError(err error, ended time.Time) error {
	if err == nil {
		return nil
	}
	lastProgress := p.lastProgress
	if lastProgress.IsZero() {
		lastProgress = p.started
	}
	return &requestBodyReadDiagnosticError{
		cause: err,
		diagnostics: BodyReadDiagnostics{
			ReceivedBytes: p.receivedBytes,
			ReadElapsedMS: max(int64(0), ended.Sub(p.started).Milliseconds()),
			ReadIdleMS:    max(int64(0), ended.Sub(lastProgress).Milliseconds()),
		},
	}
}
