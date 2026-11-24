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

// writeRequestBodyReadFailure logs the failure and writes the endpoint's error envelope.
func writeRequestBodyReadFailure(c *gin.Context, reqLog *zap.Logger, err error, write gatewayErrorWriter) {
	if write == nil {
		return
	}
	var req *http.Request
	if c != nil {
		req = c.Request
	}
	status, message := requestBodyReadFailure(reqLog, req, err)
	write(c, status, "invalid_request_error", message)
}

func requestBodyReadFailure(reqLog *zap.Logger, req *http.Request, err error) (int, string) {
	failure := pkghttputil.DescribeRequestBodyReadFailure(req, err)
	if failure.Kind == pkghttputil.RequestBodyReadErrorMaxBytes {
		return failure.Status, failure.ClientMessage()
	}
	logRequestBodyReadFailure(reqLog, req, err)
	if failure.Kind == pkghttputil.RequestBodyReadErrorTimeout {
		return failure.Status, failure.ClientMessage()
	}
	return pkghttputil.RequestBodyReadHTTPStatus(err), pkghttputil.RequestBodyReadErrorMessage(err)
}
