package handler

import (
	"net/http"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// logRequestBodyReadFailure records a bounded, payload-free reason for a body
// read failure. Clients receive a classified status/message; operators get
// enough information to distinguish compression failures from a
// disconnected/truncated upload without logging request content.
func logRequestBodyReadFailure(reqLog *zap.Logger, req *http.Request, err error) {
	if err == nil {
		return
	}
	if reqLog == nil {
		if req != nil {
			reqLog = logger.FromContext(req.Context())
		} else {
			reqLog = logger.FromContext(nil)
		}
	}

	contentLength := int64(-1)
	contentEncoding := "identity"
	if req != nil {
		contentLength = req.ContentLength
		contentEncoding = pkghttputil.RequestContentEncodingCategory(req.Header.Get("Content-Encoding"))
	}

	reqLog.Warn("read request body failed",
		zap.String("error_kind", pkghttputil.RequestBodyReadErrorKind(err)),
		zap.String("content_encoding", contentEncoding),
		zap.Int64("content_length", contentLength),
	)
}

func requestContentEncodingCategory(value string) string {
	return pkghttputil.RequestContentEncodingCategory(value)
}

func requestBodyReadErrorKind(err error) string {
	return pkghttputil.RequestBodyReadErrorKind(err)
}

type gatewayErrorWriter func(*gin.Context, int, string, string)

// writeRequestBodyReadFailure logs the bounded failure reason and writes the
// classified client response (413 / 408 / 400).
func writeRequestBodyReadFailure(c *gin.Context, reqLog *zap.Logger, err error, write gatewayErrorWriter) {
	if write == nil {
		return
	}
	if maxErr, ok := extractMaxBytesError(err); ok {
		write(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
		return
	}
	var req *http.Request
	if c != nil {
		req = c.Request
	}
	logRequestBodyReadFailure(reqLog, req, err)
	write(c, pkghttputil.RequestBodyReadHTTPStatus(err), "invalid_request_error", pkghttputil.RequestBodyReadErrorMessage(err))
}
