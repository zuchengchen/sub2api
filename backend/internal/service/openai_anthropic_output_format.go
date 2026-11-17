package service

import (
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// Conversion failures must be visible parameter errors before upstream I/O,
// including when the Messages client requested streaming.
func writeAnthropicOutputFormatValidationError(c *gin.Context, err error) bool {
	var formatErr *apicompat.AnthropicOutputFormatError
	if !errors.As(err, &formatErr) {
		return false
	}
	setOpsUpstreamError(c, http.StatusBadRequest, formatErr.Message, "")
	MarkResponseCommitted(c)
	c.JSON(http.StatusBadRequest, gin.H{
		"type": "error",
		"error": gin.H{
			"type": "invalid_request_error", "code": "unsupported_parameter",
			"param": formatErr.Param, "message": formatErr.Message,
		},
	})
	return true
}
