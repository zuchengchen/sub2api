package middleware

import (
	"net/http"

	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestbodydiag"
	"github.com/gin-gonic/gin"
)

// AbortRequestBodyReadFailure logs a bounded body-read failure and writes
// 413 / 408 / 400 in OpenAI error JSON, then aborts the chain.
func AbortRequestBodyReadFailure(c *gin.Context, err error) {
	if c == nil {
		return
	}
	if _, ok := pkghttputil.ExtractMaxBytesError(err); ok {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "Request body is too large"}})
		c.Abort()
		return
	}
	logRequestBodyReadFailure(c.Request, err)
	c.JSON(pkghttputil.RequestBodyReadHTTPStatus(err), gin.H{
		"error": gin.H{
			"type":    "invalid_request_error",
			"message": pkghttputil.RequestBodyReadErrorMessage(err),
		},
	})
	c.Abort()
}

func logRequestBodyReadFailure(req *http.Request, err error) {
	requestbodydiag.LogFailure(nil, req, err)
}
