package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIMessagesEncryptedRecoveryUsedKey = "openai_messages_encrypted_recovery_used"

// A code-less error must describe the complete upstream decryption failure.
// Neither a ciphertext prefix nor a generic signature/verification error proves
// that encrypted reasoning can be removed safely.
var openAIMessagesEncryptedRejectionPattern = regexp.MustCompile(`(?i)^the encrypted content .+ could not be verified\.\s*reason:\s*(?:encrypted content hydration failed:\s*)?encrypted content could not be decrypted or parsed\.?$`)

func openAIMessagesEncryptedRejection(status int, body []byte) bool {
	if status != http.StatusBadRequest || !gjson.ValidBytes(body) {
		return false
	}
	root := parseRawJSONView(body)
	errorObject := root.Get("error")
	if !root.IsObject() || !errorObject.IsObject() || !openAIMessagesRecoveryJSONUnambiguous(root, 0) {
		return false
	}
	if typ := root.Get("type"); typ.Exists() && typ.String() != "error" {
		return false
	}
	if typ := errorObject.Get("type"); typ.Exists() && typ.String() != "invalid_request_error" {
		return false
	}
	// This recovery/classification must never hide work, policy or access errors
	// carried alongside the validation error by an intermediary.
	var guard openAIResponseReplayGuard
	guard.Observe(body, "error")
	message := errorObject.Get("message").String()
	if guard.progressed || isOpenAIUsagePolicyError(message, body) || isOpenAIHTTPUpstreamAccessStateError(status, message, body) {
		return false
	}
	// HTTP validation errors have no metered result. Be conservative even for
	// usage schemas the shared SSE guard does not know (e.g. total_tokens only).
	for _, path := range []string{"usage", "response.usage", "data.usage", "data.response.usage"} {
		if usage := root.Get(path); usage.Exists() && usage.Type != gjson.Null {
			return false
		}
	}
	if hit, _, _ := detectOpenAICyberPolicy(body); hit {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(errorObject.Get("code").String()))
	if code == "invalid_encrypted_content" {
		return true
	}
	if code != "" && code != "thinking_signature_invalid" {
		return false
	}
	return openAIMessagesEncryptedRejectionPattern.MatchString(strings.TrimSpace(message))
}

// Conflicting duplicate JSON members are parser-dependent, so neither retry
// admission nor the request-scoped error classification may pick one of them.
func openAIMessagesRecoveryJSONUnambiguous(value gjson.Result, depth int) bool {
	if depth > 64 {
		return false
	}
	if !value.IsObject() && !value.IsArray() {
		return true
	}
	if value.IsObject() && hasDuplicateJSONObjectKeys(value) {
		return false
	}
	valid := true
	value.ForEach(func(_, child gjson.Result) bool {
		valid = openAIMessagesRecoveryJSONUnambiguous(child, depth+1)
		return valid
	})
	return valid
}

// sendOpenAIMessagesResponsesRequest repairs only an explicitly rejected HTTP
// request. It never re-enters ForwardAsAnthropic, so request synthesis, session
// identity and tool conversion run once. The gin-scoped budget also survives
// the existing account/task/continuation retry paths for this inbound request.
func (s *OpenAIGatewayService) sendOpenAIMessagesResponsesRequest(
	ctx context.Context, c *gin.Context, account *Account, request *http.Request, proxyURL string,
	diagnosticBody []byte,
) (*http.Response, error, bool) {
	// Capture the actual post-builder body before transport compression. The
	// response reader must repair this body, including any encrypted recovery,
	// rather than an earlier conversion whose input indices may differ.
	template := request.Clone(request.Context())
	var originalBody []byte
	if template.GetBody != nil && template.Header.Get("Content-Encoding") == "" {
		if reader, readErr := template.GetBody(); readErr == nil {
			originalBody, readErr = io.ReadAll(reader)
			_ = reader.Close()
			if readErr != nil {
				originalBody = nil
			}
		}
	}
	var attemptState *openAIRejectedFieldRetryAttempt
	if c != nil {
		value, _ := c.Get(openAIRejectedFieldRetryAttemptKey)
		attemptState, _ = value.(*openAIRejectedFieldRetryAttempt)
	}
	for attempt := 1; ; attempt++ {
		if attemptState != nil {
			attemptState.body = originalBody
		}
		resp, err := s.doOpenAIUpstream(request, proxyURL, account)
		if err != nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
			return resp, err, false
		}
		responseBody, _, complete := s.readOpenAIUpstreamErrorWithCompleteness(resp)
		appendOpenAIResponsesValidationDiagnostic(c, account, diagnosticBody, responseBody, resp, attempt)
		encryptedRejection := complete && openAIMessagesEncryptedRejection(resp.StatusCode, responseBody)
		var replayGuard openAIResponseReplayGuard
		replayGuard.Observe(responseBody, "error")
		canReplay := func() bool {
			return c != nil && ctx.Err() == nil && replayGuard.CanReplay(c) &&
				!IsResponseCommitted(c) && (c.Writer == nil || !c.Writer.Written()) && len(originalBody) > 0
		}
		if !complete || !canReplay() {
			return resp, nil, encryptedRejection
		}
		var retryBody []byte
		if encryptedRejection {
			if c.GetBool(openAIMessagesEncryptedRecoveryUsedKey) {
				return resp, nil, true
			}
			var stripped int
			retryBody, stripped, err = stripOpenAIMessagesRejectedReasoning(originalBody)
			if err != nil || stripped == 0 || !canReplay() {
				return resp, nil, true
			}
			// Preserve the existing one-shot .50 encrypted reasoning recovery.
			c.Set(openAIMessagesEncryptedRecoveryUsedKey, true)
			recordOpenAIMessagesEncryptedRecovery(c, account, resp, "request_recovery", stripped, len(originalBody), len(retryBody))
		} else {
			var changed bool
			retryBody, _, changed, err = normalizeOpenAIResponsesRejectedFieldRetryBody(resp.StatusCode, originalBody, responseBody)
			if err != nil {
				return resp, err, false
			}
			if !changed || !canReplay() || attemptState == nil || attemptState.state == nil || !attemptState.state.Allow(retryBody) {
				return resp, nil, false
			}
		}
		_ = resp.Body.Close()
		request = cloneOpenAIMessagesRetryRequest(template, resp, retryBody)
		originalBody = retryBody
		diagnosticBody = retryBody
	}

}

// Reuse the built request identity and transport destination. Rebuilding the
// Messages conversion would regenerate history and can reintroduce a repaired
// field or reasoning ciphertext.
func cloneOpenAIMessagesRetryRequest(template *http.Request, resp *http.Response, body []byte) *http.Request {
	retry := template.Clone(template.Context())
	if resp != nil && resp.Request != nil {
		retry.Header = resp.Request.Header.Clone()
	}
	retry.Header.Del("Content-Encoding")
	retry.Header.Del("Content-Length")
	retry.Body = io.NopCloser(bytes.NewReader(body))
	retry.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	retry.ContentLength = int64(len(body))
	return retry
}

// Patch only reasoning ciphertext in the outbound wire body. Messages, tools,
// opaque extensions and their ordering retain their original JSON bytes. A
// signature-only Messages block becomes {type:reasoning,summary:[]} after the
// Codex transform; remove that empty item rather than send an invalid skeleton.
func stripOpenAIMessagesRejectedReasoning(body []byte) ([]byte, int, error) {
	if !gjson.ValidBytes(body) {
		return body, 0, nil
	}
	root := parseRawJSONView(body)
	input := root.Get("input")
	if !root.IsObject() || hasDuplicateJSONObjectKeys(root) || !input.IsArray() {
		return body, 0, nil
	}
	items := input.Array()
	referenced := make(map[string]bool)
	for _, item := range items {
		if item.IsObject() && hasDuplicateJSONObjectKeys(item) {
			return body, 0, nil
		}
		if item.Get("type").String() == "item_reference" {
			referenced[item.Get("id").String()] = true
		}
	}
	stripped := 0
	patched := make([]string, 0, len(items))
	for _, item := range items {
		if item.Get("type").String() != "reasoning" || item.Get("encrypted_content").Type != gjson.String ||
			strings.TrimSpace(item.Get("encrypted_content").String()) == "" {
			patched = append(patched, item.Raw)
			continue
		}
		if id := item.Get("id").String(); id != "" && referenced[id] {
			return body, 0, nil
		}
		next, err := sjson.DeleteBytes([]byte(item.Raw), "encrypted_content")
		if err != nil {
			return nil, 0, err
		}
		empty := true
		parseRawJSONView(next).ForEach(func(key, value gjson.Result) bool {
			if key.String() == "type" || (key.String() == "summary" && (value.Type == gjson.Null || (value.IsArray() && len(value.Array()) == 0))) {
				return true
			}
			empty = false
			return false
		})
		if !empty {
			patched = append(patched, string(next))
		}
		stripped++
	}
	if stripped == 0 {
		return body, 0, nil
	}
	return replaceOpenAIRawInput(body, input, patched), stripped, nil
}

// A rejected request must not become a credential failure through custom
// cooldown policies. Keep the client-facing error, but never persist ciphertext
// from its message in recovery/account diagnostics.
func handleOpenAIMessagesEncryptedRejection(ctx context.Context, c *gin.Context, account *Account, resp *http.Response, message string) (*OpenAIForwardResult, error) {
	recordOpenAIMessagesEncryptedRecovery(c, account, resp, "http_error", 0, 0, 0)
	setOpsUpstreamError(c, http.StatusBadRequest, "Upstream rejected encrypted reasoning content", "")
	if strings.TrimSpace(message) == "" {
		message = "Upstream rejected encrypted reasoning content"
	}
	if c != nil && ctx.Err() == nil && (c.Request == nil || c.Request.Context().Err() == nil) &&
		!IsResponseCommitted(c) && c.Writer != nil && !c.Writer.Written() {
		MarkResponseCommitted(c)
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", message)
	}
	return nil, fmt.Errorf("upstream encrypted reasoning content rejected: %d", http.StatusBadRequest)
}

func recordOpenAIMessagesEncryptedRecovery(c *gin.Context, account *Account, resp *http.Response, kind string, stripped, before, after int) {
	if c == nil || account == nil || resp == nil {
		return
	}
	attempts := 0
	if c.GetBool(openAIMessagesEncryptedRecoveryUsedKey) {
		attempts = 1
	}
	detail, _ := json.Marshal(struct {
		RecoveryAttempts int    `json:"recovery_attempts"`
		StrippedItems    int    `json:"stripped_reasoning_items,omitempty"`
		RequestBytes     int    `json:"request_bytes,omitempty"`
		RetryBytes       int    `json:"retry_bytes,omitempty"`
		Transport        string `json:"transport"`
	}{attempts, stripped, before, after, "http"})
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
		Kind: kind, Stage: "messages_responses", Scope: "request", Reason: "invalid_encrypted_reasoning",
		Message: "Upstream rejected encrypted reasoning content", Detail: string(detail),
	})
}
