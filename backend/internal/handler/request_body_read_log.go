package handler

import (
	"net/http"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestbodydiag"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// logRequestBodyReadFailure records a bounded, payload-free reason for a body
// read failure. Clients receive a classified status/message; operators get
// enough information to distinguish compression failures from a
// disconnected/truncated upload without logging request content.
func logRequestBodyReadFailure(reqLog *zap.Logger, req *http.Request, err error) {
	requestbodydiag.LogFailure(reqLog, req, err)
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
