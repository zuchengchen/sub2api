package service

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// OpenAIContinuationContextError reports missing context without disclosing
// call IDs, response IDs, tool results, or credential ownership metadata.
type OpenAIContinuationContextError struct {
	Param string
}

func (e *OpenAIContinuationContextError) Error() string {
	if e.Param == "conversation" {
		return "The stored conversation context cannot be preserved on another account. Continue with its original credential or send complete inline history without conversation."
	}
	if e.Param == "previous_response_id" {
		return "The previous response context cannot be preserved on this route. Continue on the original connection or send the complete conversation history without previous_response_id."
	}
	return "Tool output requires its matching preceding call or valid continuation context. Include the complete call and result history or continue on the original response."
}

// ValidateOpenAIContinuationInput validates only stateless tool dependencies.
// Native anchors/references retain their upstream resolution and existing
// tenant/credential checks. Neither is proof of portable, complete history.
func ValidateOpenAIContinuationInput(body []byte) error {
	proof := AnalyzeOpenAIContinuationReplayProof(body)
	if proof.HasPreviousResponseID || proof.HasConversation || proof.HasItemReference {
		return nil
	}
	// Bounds and unfamiliar state decline replay proofs rather than inventing
	// request limits. The upstream still validates these unchanged requests.
	if !proof.HasToolOutput || !proof.ToolContextUnproven {
		return nil
	}
	return &OpenAIContinuationContextError{Param: "input"}
}

func writeOpenAIContinuationContextError(c *gin.Context, err error) {
	param := "input"
	if contextErr, ok := err.(*OpenAIContinuationContextError); ok {
		param = contextErr.Param
	}
	writeOpenAIResponsesErrorJSON(c, http.StatusBadRequest, gin.H{"error": gin.H{
		"type": "invalid_request_error", "code": "continuation_context_missing", "param": param, "message": err.Error(),
	}})
}

func WriteOpenAIContinuationContextError(c *gin.Context, err error) {
	writeOpenAIContinuationContextError(c, err)
}

func NewOpenAIContinuationContextError(payload []byte) *OpenAIContinuationContextError {
	param := "input"
	if previous := gjson.GetBytes(payload, "previous_response_id"); previous.Type == gjson.String && previous.String() != "" {
		param = "previous_response_id"
	} else if conversation := gjson.GetBytes(payload, "conversation"); conversation.Exists() && conversation.Type != gjson.Null {
		param = "conversation"
	}
	return &OpenAIContinuationContextError{Param: param}
}

// OpenAIContinuationMaySwitchAccount is a dependency/anchor boundary only.
// Existing no-progress, output, cancellation, ownership and retry-budget gates
// still apply. An explicit native anchor is never portable on its own.
func OpenAIContinuationMaySwitchAccount(payload []byte) bool {
	proof := AnalyzeOpenAIContinuationReplayProof(payload)
	return !proof.HasPreviousResponseID && !proof.HasConversation && openAIContinuationProofPermitsReplay(proof)
}

func openAIContinuationProofPermitsReplay(proof OpenAIContinuationReplayProof) bool {
	return proof.Verdict == OpenAIContinuationProofInline || proof.Verdict == OpenAIContinuationProofNoDependency
}

// A first request without an anchor defines a new root, but references or
// opaque state can still prevent reconstructing it on another credential.
func openAIContinuationStartsCompleteHistory(payload []byte) bool {
	proof := analyzeOpenAIContinuationReplayProof(payload, true)
	return !proof.HasPreviousResponseID && !proof.HasConversation && openAIContinuationProofPermitsReplay(proof)
}

// completeHistory must come from a root and an unbroken chain observed on this
// connection, not merely from the presence of input. The caller also keeps the
// credential/tenant owner fixed. Portable checks decline all opaque references;
// same-owner checks may retain complete encrypted reasoning from that owner.
func openAIContinuationReplayInputProof(payload []byte, input []json.RawMessage, completeHistory, sameOwner bool) bool {
	if !completeHistory {
		return false
	}
	withInput, err := setOpenAIWSPayloadInputSequence(payload, input, true)
	if err != nil {
		return false
	}
	proof := analyzeOpenAIContinuationReplayProof(withInput, sameOwner)
	return openAIContinuationProofPermitsReplay(proof)
}

func openAIContinuationMatchesObservedResponse(payload []byte, responseID string) bool {
	previous := gjson.GetBytes(payload, "previous_response_id")
	return previous.Type == gjson.String && responseID != "" && previous.String() == strings.TrimSpace(responseID)
}

func openAIContinuationReplayAnnotationPreservesContent(before, after []json.RawMessage) bool {
	if len(before) != len(after) {
		return false
	}
	for i, item := range before {
		content := gjson.GetBytes(item, "content")
		if content.Exists() && content.Raw != gjson.GetBytes(after[i], "content").Raw {
			return false
		}
	}
	return true
}
