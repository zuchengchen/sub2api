package middleware

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestbodydiag"
	"github.com/gin-gonic/gin"
)

// AbortWithRequestBodyReadFailure writes the shared response for a failed body
// read in middleware that inspects the body before the handler. It applies the
// same classification, client message, protocol-shaped error body and
// payload-free diagnostics as the gateway handlers, so a body read in
// middleware fails exactly as it would in the handler.
func AbortWithRequestBodyReadFailure(c *gin.Context, component string, err error) {
	failure := httputil.DescribeRequestBodyReadFailure(c.Request, err)
	if failure.Kind != httputil.RequestBodyReadErrorMaxBytes {
		requestbodydiag.LogFailure(nil, c.Request, err)
	}
	status, message := failure.Status, failure.ClientMessage()
	switch failure.Kind {
	case httputil.RequestBodyReadErrorTruncated, httputil.RequestBodyReadErrorTransport:
		status = httputil.RequestBodyReadHTTPStatus(err)
		message = httputil.RequestBodyReadErrorMessage(err)
	}
	writeGatewayInvalidRequest(c, status, message)
	c.Abort()
}
