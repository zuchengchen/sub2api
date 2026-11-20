package service

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"golang.org/x/net/http/httpguts"
)

const responsesCacheAffinityScopeEnv = "SUB2_RESPONSES_CACHE_AFFINITY_SCOPE"
const responsesCacheAffinityContextKey = "openai_responses_cache_affinity"

func responsesCacheAffinityEnabled(c *gin.Context, textModel string) bool {
	if textModel == "" {
		return false
	}
	scope := os.Getenv(responsesCacheAffinityScopeEnv)
	if scope == "" {
		return false
	}
	key := gptAccountConversationKey(c, textModel)
	return key != nil && (scope == "all" || scope == "key:"+strconv.FormatInt(key.ID, 10))
}

// Native WS handshakes carry one identity across turns that may change models.
// Apply the additional Claude/turn projection only after an HTTP account and
// its actual upstream model are known; WS retains its connection-level rules.
func useResponsesClaudeHTTPIdentity(c *gin.Context, account *Account, root gjson.Result) bool {
	if !c.GetBool(responsesCacheAffinityContextKey) || c.Request.Method != http.MethodPost ||
		GetOpenAIClientTransport(c) == OpenAIClientTransportWS || !isCodexOAuthAccount(account) ||
		account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() {
		return false
	}
	requested := root.Get("model").Str
	return requested != "" && isCanonicalGPTChatModel(resolveOpenAIAccountUpstreamModelForRequest(account, requested, isOpenAIResponsesCompactPath(c)))
}

// Accept a bounded, single Claude Code session header or a session UUID from
// its structured metadata.user_id. A bare user ID, a suffix on arbitrary text,
// and conflicting carriers cannot establish a conversation.
func validatedResponsesClaudeSession(c *gin.Context, body []byte) string {
	if c == nil || c.Request == nil || len(body) > gpt61SolChatIdentityMaxBodyBytes {
		return ""
	}
	headers := c.Request.Header.Values(claudeCodeSessionHeader)
	metadata := parseRawJSONView(body).Get("metadata")
	if len(headers) == 0 && !metadata.Get("user_id").Exists() {
		return ""
	}
	header, valid := validatedResponsesClaudeHeader(headers)
	if !valid {
		return ""
	}
	bodyID, valid := validatedResponsesClaudeMetadata(metadata)
	if !valid || (header != "" && bodyID != "" && header != bodyID) {
		return ""
	}
	session := firstNonEmptyString(header, bodyID)
	if session == "" {
		return ""
	}
	identity := parseGPT61SolChatIdentity(c, body, false)
	if !identity.valid || identity.evidence.ClientSessionID != session ||
		identity.evidence.ClientThreadID != "" || identity.evidence.ParentThreadID != "" {
		return ""
	}
	return session
}

func validatedResponsesClaudeHeader(values []string) (string, bool) {
	if len(values) == 0 {
		return "", true
	}
	if len(values) != 1 || len(values[0]) > maxPersistedSessionIDLength {
		return "", false
	}
	session := sanitizeGPT61SolChatIdentity(values[0])
	// The HTTP token grammar excludes quoted strings and comma-joined values.
	if !httpguts.ValidHeaderFieldName(session) || session == uuid.Nil.String() {
		return "", false
	}
	return session, true
}

func validatedResponsesClaudeMetadata(metadata gjson.Result) (string, bool) {
	if !metadata.Exists() || metadata.Type == gjson.Null {
		return "", true
	}
	metadata, valid := gpt61SolChatIdentityMetadata(metadata.Raw)
	if !valid {
		return "", false
	}
	user := metadata.Get("user_id")
	if !user.Exists() || user.Type == gjson.Null {
		return "", true
	}
	if user.Type != gjson.String {
		return "", false
	}
	raw := strings.TrimSpace(user.Str)
	session := ""
	switch {
	case strings.HasPrefix(raw, "{"):
		nested, valid := gpt61SolChatIdentityMetadata(raw)
		if !valid {
			return "", false
		}
		id := nested.Get("session_id")
		if !id.Exists() {
			return "", true
		}
		session = id.Str
	case strings.Contains(raw, "_session_"):
		fields := legacyUserIDRegex.FindStringSubmatch(raw)
		if len(fields) != 4 || (fields[2] != "" && !isCanonicalUUID(fields[2])) {
			return "", false
		}
		session = fields[3]
	default:
		return "", true
	}
	if session != strings.TrimSpace(session) || !isCanonicalUUID(session) || session == uuid.Nil.String() {
		return "", false
	}
	return session, true
}
