package service

import (
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	responsesStatelessCacheModeEnv       = "SUB2_RESPONSES_STATELESS_CACHE_MODE"
	responsesStatelessCacheUntilEnv      = "SUB2_RESPONSES_STATELESS_CACHE_TRIAL_UNTIL"
	responsesStatelessCacheRequestKey    = "openai_responses_stateless_cache_request"
	responsesStatelessCacheAssignmentKey = "openai_responses_stateless_cache_assignment"
	responsesStatelessCacheActionKey     = "openai_responses_stateless_cache_action"
)

// This evidence belongs to one authenticated HTTP request, including its retries.
// The explicit cache key is only compared with its ordinary outbound projection.
type responsesStatelessCacheRequest struct {
	userID, apiKeyID int64
	cacheKey, arm    string
	until            time.Time
}

// Stage the original input before the gateway adds anonymous session metadata or
// synthesizes Codex input. Only the native HTTP Responses handler calls this hook.
func stageResponsesStatelessCacheTrial(c *gin.Context, body []byte, textModel string) {
	mode := os.Getenv(responsesStatelessCacheModeEnv)
	if mode == "" || c == nil || c.Request == nil || c.Request.URL == nil ||
		c.Request.Method != http.MethodPost || c.Request.URL.Path != "/v1/responses" ||
		GetOpenAIClientTransport(c) != OpenAIClientTransportHTTP || isOpenAINativeCompactionV2(c) {
		return
	}
	if image, _ := getOpenAIImageIntentHint(c); image {
		return
	}
	key := gptAccountConversationKey(c, textModel)
	if key == nil || openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator")) {
		return
	}
	if _, staged := c.Get(responsesStatelessCacheRequestKey); staged {
		return
	}
	prefix := "key:" + strconv.FormatInt(key.ID, 10) + ":"
	var until time.Time
	arm := "fixed_full"
	switch mode {
	case prefix + "trial":
		raw := os.Getenv(responsesStatelessCacheUntilEnv)
		end, err := time.Parse(time.RFC3339, raw)
		if err != nil || !strings.HasSuffix(raw, "Z") || !time.Now().Before(end) {
			return
		}
		until = end
	case prefix + "full":
	default:
		return
	}
	cacheKey := responsesStatelessCacheKey(c, body)
	if cacheKey == "" {
		return
	}
	if !until.IsZero() {
		arm = "control_prewarm"
		if rand.IntN(2) != 0 {
			arm = "trial_full"
		}
	}
	c.Set(responsesStatelessCacheRequestKey, &responsesStatelessCacheRequest{
		userID: key.UserID, apiKeyID: key.ID, cacheKey: strings.Clone(cacheKey), arm: arm, until: until,
	})
}

func responsesStatelessCacheKey(c *gin.Context, body []byte) string {
	if len(body) == 0 || len(body) > gpt61SolChatIdentityMaxBodyBytes || !utf8.Valid(body) || !gjson.ValidBytes(body) {
		return ""
	}
	for _, name := range explicitOpenAIHeaderSessionNames {
		if len(c.Request.Header.Values(name)) > 0 {
			return ""
		}
	}
	for _, name := range []string{claudeCodeSessionHeader, "thread-id", "turn-id", "x-codex-parent-thread-id", openAIWSTurnStateHeader,
		openAIWSTurnMetadataHeader, openAIWSWindowIDHeader, "x-openai-subagent", "x-openai-memgen-request"} {
		if len(c.Request.Header.Values(name)) > 0 {
			return ""
		}
	}
	fields := responsesCacheDiagnosticObject(parseRawJSONView(body))
	pck := fields["prompt_cache_key"]
	if pck.Type != gjson.String || pck.Str == "" || len(pck.Str) > maxPersistedSessionIDLength ||
		strings.TrimSpace(pck.Str) != pck.Str || strings.IndexFunc(pck.Str, unicode.IsControl) >= 0 {
		return ""
	}
	for _, name := range []string{"conversation", "previous_response_id", "conversation_id", "session_id", "thread_id",
		"parent_thread_id", "forked_from_thread_id", "turn_id", "root_turn_id", "parent_turn_id", "prompt", "generate",
		"compaction_trigger", "context_management", openAIWSTurnMetadataHeader, openAIWSTurnStateHeader} {
		if _, exists := fields[name]; exists {
			return ""
		}
	}
	if fields["background"].Bool() || fields["store"].Bool() {
		return ""
	}
	if metadata, exists := fields["client_metadata"]; exists {
		if entries := responsesCacheDiagnosticObject(metadata); entries == nil || len(entries) > 0 {
			return ""
		}
	}
	if metadata, exists := fields["metadata"]; exists {
		entries := responsesCacheDiagnosticObject(metadata)
		if entries == nil {
			return ""
		}
		for name, value := range entries {
			// Ordinary Platform metadata is string-to-string. Known bridge IDs
			// and structured identity are deliberately outside this experiment.
			if value.Type != gjson.String || name == "user_id" || strings.Contains(name, "session") ||
				strings.Contains(name, "thread") || strings.Contains(name, "conversation") ||
				strings.Contains(name, "turn") || strings.Contains(name, "state") {
				return ""
			}
		}
	}
	if !responsesStatelessCacheCompleteInput(fields["input"]) {
		return ""
	}
	return pck.Str
}

func responsesStatelessCacheCompleteInput(input gjson.Result) bool {
	if input.Type == gjson.String {
		return input.Str != ""
	}
	if !input.IsArray() {
		return false
	}
	var calls []gjson.Result
	count := 0
	valid := true
	input.ForEach(func(_, item gjson.Result) bool {
		count++
		fields := responsesCacheDiagnosticObject(item)
		if count > openAIWSDeltaChainMaxBaseline || fields == nil {
			valid = false
			return false
		}
		if kind := fields["type"]; kind.Exists() && kind.Type != gjson.String {
			valid = false
			return false
		}
		if status := fields["status"]; status.Exists() && status.Type != gjson.Null && status.Str != "completed" {
			valid = false
			return false
		}
		if fields["type"].Str != "reasoning" && openAIContinuationProofHasOpaqueState(item) {
			valid = false
			return false
		}
		switch fields["type"].Str {
		case "", "message":
			switch fields["role"].Str {
			case "user", "assistant", "developer", "system":
				valid = responsesStatelessCacheInlineText(fields["content"])
			default:
				valid = false
			}
		case "function_call", "custom_tool_call":
			valid = openAIContinuationProofCallComplete(item, fields["type"].Str)
			calls = append(calls, item)
		case "function_call_output", "custom_tool_call_output":
			valid = responsesStatelessCacheInlineText(fields["output"])
			calls = append(calls, item)
		case "reasoning":
			valid = responsesStatelessCacheInlineReasoning(item, fields)
		default:
			// Hosted items, file references and compacted history use
			// the ordinary forwarder. Only complete inline histories opt in.
			valid = false
		}
		return valid
	})
	return valid && count > 0 && openAIWSContinuationToolsComplete(calls, nil)
}

func responsesStatelessCacheInlineReasoning(item gjson.Result, fields responsesCacheDiagnosticFields) bool {
	summary := fields["summary"]
	if !summary.IsArray() {
		return false
	}
	valid, hasText := true, false
	summary.ForEach(func(_, part gjson.Result) bool {
		p := responsesCacheDiagnosticObject(part)
		valid = p["type"].Str == "summary_text" && p["text"].Type == gjson.String
		hasText = hasText || p["text"].Str != ""
		return valid
	})
	if !valid {
		return false
	}
	id := fields["id"]
	if id.Exists() && id.Type != gjson.Null && !openAIContinuationProofString(id, true) {
		return false
	}
	if cipher := fields["encrypted_content"]; cipher.Exists() && cipher.Type != gjson.Null {
		return openAIContinuationProofOwnedOpaqueItem(item, "reasoning")
	}
	// Plain summary text without an ID is ordinary inline data. An ID alone
	// can ask the upstream to recover hidden state. Never infer that state.
	return hasText && (id.Type == gjson.Null || !id.Exists()) && !openAIContinuationProofHasOpaqueState(item)
}

func responsesStatelessCacheInlineText(value gjson.Result) bool {
	if value.Type == gjson.String {
		return true
	}
	if !value.IsArray() {
		return false
	}
	valid := false
	value.ForEach(func(_, part gjson.Result) bool {
		fields := responsesCacheDiagnosticObject(part)
		kind := fields["type"].Str
		valid = (kind == "input_text" || kind == "output_text") && fields["text"].Type == gjson.String
		return valid
	})
	return valid
}

type responsesStatelessCacheAttempt struct {
	arm     string
	skipped bool
}

func responsesStatelessCacheForAttempt(c *gin.Context, account *Account, payload map[string]any, executionScope string, cookieWS bool) *responsesStatelessCacheAttempt {
	if c == nil {
		return nil
	}
	raw, ok := c.Get(responsesStatelessCacheRequestKey)
	trial, _ := raw.(*responsesStatelessCacheRequest)
	if !ok || trial == nil || executionScope != "" || cookieWS || !isCodexOAuthAccount(account) ||
		account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() {
		return nil
	}
	key := gptAccountConversationKey(c, openAIWSPayloadString(payload, "model"))
	if key == nil || key.ID != trial.apiKeyID || key.UserID != trial.userID ||
		(!trial.until.IsZero() && !time.Now().Before(trial.until) && c.GetString(responsesStatelessCacheAssignmentKey) == "") {
		return nil
	}
	owner := codexAccountIdentitySource(c, account)
	if openAIWSPayloadString(payload, "prompt_cache_key") !=
		scopeCodexAccountIdentityValue(owner, key.ID, "prompt-cache", trial.cacheKey) {
		return nil
	}
	return &responsesStatelessCacheAttempt{arm: trial.arm}
}

// Assignment includes eligible prewarm handshakes even when dialing fails.
// Action requires a successful write, after the fresh-lease and reservation gates.
func responsesStatelessCacheTrialObservation(c *gin.Context) (assignment, action string) {
	if c != nil {
		return c.GetString(responsesStatelessCacheAssignmentKey), c.GetString(responsesStatelessCacheActionKey)
	}
	return "", ""
}
