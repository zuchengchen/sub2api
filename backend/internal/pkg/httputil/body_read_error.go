package httputil

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"

	"github.com/Wei-Shaw/sub2api/internal/pkg/userfacing"
)

const (
	BodyReadKindNone                       = "none"
	BodyReadKindMaxBytes                   = "max_bytes"
	BodyReadKindUnsupportedContentEncoding = "unsupported_content_encoding"
	BodyReadKindDecodeContentEncoding      = "decode_content_encoding"
	BodyReadKindClientDisconnect           = "client_disconnect"
	BodyReadKindTruncatedBody              = "truncated_body"
	BodyReadKindTransport                  = "transport"
	BodyReadKindIORead                     = "io_read"
)

// ExtractMaxBytesError unwraps a MaxBytesError if present.
func ExtractMaxBytesError(err error) (*http.MaxBytesError, bool) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return maxErr, true
	}
	return nil, false
}

// RequestContentEncodingCategory maps a Content-Encoding header to a small
// set of labels safe to log (no raw header values).
func RequestContentEncodingCategory(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "identity":
		return "identity"
	case "gzip", "x-gzip":
		return "gzip"
	case "zstd":
		return "zstd"
	case "deflate":
		return "deflate"
	default:
		return "other"
	}
}

// RequestBodyReadErrorKind classifies a body-read failure without including
// request payload. Used by operators to distinguish compression failures from
// a disconnected or truncated upload.
func RequestBodyReadErrorKind(err error) string {
	if err == nil {
		return BodyReadKindNone
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return BodyReadKindMaxBytes
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "decode content-encoding") {
		if strings.Contains(lower, "unsupported content-encoding") {
			return BodyReadKindUnsupportedContentEncoding
		}
		return BodyReadKindDecodeContentEncoding
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) {
		return BodyReadKindClientDisconnect
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return BodyReadKindTruncatedBody
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return BodyReadKindTransport
	}
	switch {
	case strings.Contains(lower, "i/o timeout"):
		return BodyReadKindTransport
	case strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "broken pipe"),
		strings.Contains(lower, "use of closed network connection"),
		strings.Contains(lower, "client disconnected"),
		strings.Contains(lower, "stream closed"):
		return BodyReadKindClientDisconnect
	}
	return BodyReadKindIORead
}

// RequestBodyReadIncomplete reports whether the failure is a client abort,
// truncated upload, or transport timeout rather than a malformed body.
func RequestBodyReadIncomplete(err error) bool {
	switch RequestBodyReadErrorKind(err) {
	case BodyReadKindClientDisconnect, BodyReadKindTruncatedBody, BodyReadKindTransport:
		return true
	default:
		return false
	}
}

// RequestBodyReadHTTPStatus maps a body-read error to the client HTTP status.
func RequestBodyReadHTTPStatus(err error) int {
	switch RequestBodyReadErrorKind(err) {
	case BodyReadKindNone:
		return http.StatusOK
	case BodyReadKindMaxBytes:
		return http.StatusRequestEntityTooLarge
	case BodyReadKindClientDisconnect, BodyReadKindTruncatedBody, BodyReadKindTransport:
		return http.StatusRequestTimeout
	default:
		return http.StatusBadRequest
	}
}

// RequestBodyReadErrorMessage is the user-facing message for a classified
// body-read failure. Callers that know a MaxBytesError limit should replace
// the max-bytes message with a limit-specific one.
func RequestBodyReadErrorMessage(err error) string {
	switch RequestBodyReadErrorKind(err) {
	case BodyReadKindMaxBytes:
		return "Request body is too large"
	case BodyReadKindClientDisconnect, BodyReadKindTruncatedBody, BodyReadKindTransport:
		return userfacing.RequestBodyIncomplete
	default:
		return userfacing.FailedToReadBody
	}
}
