package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func isOpenAIImageGenerationUserErrorType(errType string) bool {
	switch strings.ToLower(strings.TrimSpace(errType)) {
	case "image_generation_user_error", "image_generation_user_err":
		return true
	default:
		return false
	}
}

// Image user errors describe the request, not credential health or a transient
// outage. Recognize both terminal envelopes without inferring from prose.
func openAIImageGenerationUserError(payload []byte) *OpenAIImagesUpstreamError {
	for _, path := range []string{"response.error", "error"} {
		value := gjson.GetBytes(payload, path)
		if isOpenAIImageGenerationUserErrorType(value.Get("type").String()) {
			return openAIImagesUpstreamErrorFromGJSON(value, "")
		}
	}
	return nil
}

func markOpenAIImageGenerationStreamError(c *gin.Context, payload []byte) {
	if upstreamErr := openAIImageGenerationUserError(payload); upstreamErr != nil {
		setOpsUpstreamError(c, http.StatusBadRequest, upstreamErr.clientMessage(), "")
		MarkOpsStreamFailure(c, upstreamErr.ErrorType, upstreamErr.Code, upstreamErr.clientMessage(), http.StatusBadRequest)
		MarkResponseCommitted(c)
	}
}

func writeOpenAIImageGenerationUserError(c *gin.Context, upstreamErr *OpenAIImagesUpstreamError) {
	setOpsUpstreamError(c, http.StatusBadRequest, upstreamErr.clientMessage(), "")
	MarkResponseCommitted(c)
	errorBody := gin.H{"type": upstreamErr.clientErrorType(), "message": upstreamErr.clientMessage()}
	if upstreamErr.Code != "" {
		errorBody["code"] = upstreamErr.Code
	}
	if upstreamErr.Param != "" {
		errorBody["param"] = upstreamErr.Param
	}
	if StopOpenAICompactSSEKeepaliveCommitted(c) {
		MarkOpsStreamFailure(c, upstreamErr.ErrorType, upstreamErr.Code, upstreamErr.clientMessage(), http.StatusBadRequest)
		writeOpenAIResponsesSSEFailure(c, http.StatusBadRequest, errorBody)
		return
	}
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusBadRequest, gin.H{"error": errorBody})
}
