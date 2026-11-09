package service

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const gpt61SolChatIdentityMaxBodyBytes = 32 << 20
const gpt61SolChatIdentityMaxMetadataBytes = 16 << 10

var gpt61SolChatIdentityFields = []string{
	"session_id", "thread_id", "turn_id", "parent_thread_id", "x-codex-parent-thread-id",
	"forked_from_thread_id", "root_turn_id", "parent_turn_id",
}

type gpt61SolChatIdentity struct {
	evidence      codexTurnEvidence
	valid         bool
	sessionSource string
	threadSource  string
	turnSource    string
}

// Match persisted session limits, but do not trim away otherwise invalid control
// characters before validating a new identity source.
func sanitizeGPT61SolChatIdentity(raw string) string {
	if !utf8.ValidString(raw) || strings.IndexFunc(raw, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return ""
	}
	return sanitizeSessionID(raw)
}

func gpt61SolChatIdentityMetadata(raw string) (gjson.Result, bool) {
	if len(raw) > gpt61SolChatIdentityMaxMetadataBytes || !utf8.ValidString(raw) {
		return gjson.Result{}, false
	}
	canonical, err := nativeReplayCanonicalObject([]byte(raw))
	if err != nil {
		return gjson.Result{}, false
	}
	value := gjson.ParseBytes(canonical)
	for _, field := range gpt61SolChatIdentityFields {
		v := value.Get(field)
		if !v.Exists() || v.Type == gjson.Null {
			continue
		}
		if v.Type != gjson.String || (v.Str != "" && strings.Trim(v.Str, " ") != "" && sanitizeGPT61SolChatIdentity(v.Str) == "") {
			return gjson.Result{}, false
		}
	}
	if v := value.Get("turn_started_at_unix_ms"); v.Exists() && v.Type != gjson.Null {
		if v.Type != gjson.String && v.Type != gjson.Number {
			return gjson.Result{}, false
		}
		ms, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil || ms < 0 {
			return gjson.Result{}, false
		}
	}
	return value, true
}

func gpt61SolChatIdentityHeaders() []string {
	headers := append([]string(nil), clientSessionIDHeaders...)
	return append(headers, "thread-id", "turn-id", "x-codex-parent-thread-id")
}

// Keep the legacy Claude metadata carrier explicit and bounded as well.
func gpt61SolChatClaudeSession(root gjson.Result) (string, bool) {
	raw := root.Get("metadata")
	if !raw.Exists() || raw.Type == gjson.Null {
		return "", true
	}
	meta, valid := gpt61SolChatIdentityMetadata(raw.Raw)
	if !valid {
		return "", false
	}
	user := meta.Get("user_id")
	if !user.Exists() || user.Type == gjson.Null {
		return "", true
	}
	if user.Type != gjson.String {
		return "", false
	}
	value := strings.TrimSpace(user.Str)
	if matches := claudeCodeSessionSuffixPattern.FindStringSubmatch(value); len(matches) >= 2 {
		id := sanitizeGPT61SolChatIdentity(matches[1])
		return id, id != ""
	}
	if strings.HasPrefix(value, "{") {
		nested, valid := gpt61SolChatIdentityMetadata(value)
		if !valid {
			return "", false
		}
		id := nested.Get("session_id")
		if !id.Exists() || id.Type == gjson.Null {
			return "", true
		}
		session := sanitizeGPT61SolChatIdentity(id.Str)
		return session, session != ""
	}
	return "", true
}

// One parser supplies both the target-model identity bridge and replay scope.
// It preserves collectCodexTurnEvidence precedence. Invalid/ambiguous recognized
// metadata fails closed instead of interpreting a cache hint as a session.
func resolveGPT61SolChatIdentity(c *gin.Context, body []byte) gpt61SolChatIdentity {
	result := gpt61SolChatIdentity{}
	if c == nil || c.Request == nil || len(body) > gpt61SolChatIdentityMaxBodyBytes || !utf8.Valid(body) || !gjson.ValidBytes(body) {
		return result
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() || hasDuplicateJSONObjectKeys(root) {
		return result
	}
	bodyMeta, embeddedBody, headerMeta := gjson.Result{}, gjson.Result{}, gjson.Result{}
	if raw := root.Get("client_metadata"); raw.Exists() && raw.Type != gjson.Null {
		var ok bool
		bodyMeta, ok = gpt61SolChatIdentityMetadata(raw.Raw)
		if !ok {
			return result
		}
		if embedded := bodyMeta.Get(gjsonEscapeKey(openAIWSTurnMetadataHeader)); embedded.Exists() && embedded.Type != gjson.Null {
			if embedded.Type != gjson.String {
				return result
			}
			if strings.TrimSpace(embedded.Str) != "" {
				embeddedBody, ok = gpt61SolChatIdentityMetadata(embedded.Str)
				if !ok {
					return result
				}
			}
		}
	}
	headers := c.Request.Header
	for _, name := range append(gpt61SolChatIdentityHeaders(), openAIWSTurnMetadataHeader) {
		if len(headers.Values(name)) > 1 {
			return result
		}
		if name == openAIWSTurnMetadataHeader {
			continue
		}
		if raw := headers.Get(name); raw != "" && strings.Trim(raw, " ") != "" && sanitizeGPT61SolChatIdentity(raw) == "" {
			return result
		}
	}
	if raw := strings.TrimSpace(headers.Get(openAIWSTurnMetadataHeader)); raw != "" {
		var ok bool
		headerMeta, ok = gpt61SolChatIdentityMetadata(raw)
		if !ok {
			return result
		}
	}
	result.evidence = collectCodexTurnEvidence(bodyMeta, headers, root)
	ev := &result.evidence
	if ev.ClientSessionID == "" && ev.ClientThreadID == "" {
		for _, name := range clientSessionIDHeaders {
			if session := sanitizeGPT61SolChatIdentity(headers.Get(name)); session != "" {
				ev.ClientSessionID = session
				result.sessionSource = "header:" + strings.ToLower(name)
				break
			}
		}
		if ev.ClientSessionID == "" {
			session, valid := gpt61SolChatClaudeSession(root)
			if !valid {
				return gpt61SolChatIdentity{}
			}
			if session != "" {
				ev.ClientSessionID = session
				result.sessionSource = "body:claude_metadata_session"
			}
		}
	}
	sourceFor := func(field, selected string) string {
		if selected == "" {
			return "none"
		}
		for _, source := range []struct {
			name  string
			value gjson.Result
		}{
			{"body:turn_metadata", embeddedBody}, {"body:client_metadata", bodyMeta}, {"header:turn_metadata", headerMeta},
		} {
			if source.value.Get(field).Type == gjson.String && strings.TrimSpace(source.value.Get(field).Str) == selected {
				return source.name
			}
		}
		if field == "session_id" {
			if strings.TrimSpace(headers.Get("session-id")) == selected {
				return "header:session-id"
			}
			if strings.TrimSpace(headers.Get("session_id")) == selected {
				return "header:session_id"
			}
		}
		if field == "thread_id" {
			return "header:thread-id"
		}
		if field == "turn_id" {
			return "header:turn-id"
		}
		return "none"
	}
	if result.sessionSource == "" {
		result.sessionSource = sourceFor("session_id", ev.ClientSessionID)
	}
	result.threadSource = sourceFor("thread_id", ev.ClientThreadID)
	result.turnSource = sourceFor("turn_id", ev.ClientTurnID)
	result.valid = true
	return result
}

// The caller still binds authenticated user/Key/credential/model and the complete
// history. A turn-only signal and prompt_cache_key do not establish a replayable
// conversation. Keep session and thread separate when constructing a scope key.
func gpt61SolChatExplicitIdentity(c *gin.Context, body []byte) (session, thread string, valid bool) {
	resolved := resolveGPT61SolChatIdentity(c, body)
	if !resolved.valid {
		return "", "", false
	}
	session = firstNonEmptyString(resolved.evidence.ClientSessionID, resolved.evidence.ClientThreadID)
	thread = resolved.evidence.ClientThreadID
	if thread == "" && resolved.evidence.ParentThreadID != "" {
		// Match the fingerprint layer: a parent proves this is a child, but
		// cannot identify which child. An explicit turn can identify it;
		// otherwise the wire identity is anonymous and must not be replayed.
		thread = resolved.evidence.ClientTurnID
		if thread == "" {
			return "", "", false
		}
	}
	thread = firstNonEmptyString(thread, session)
	return session, thread, session != "" && thread != ""
}

// enabled is a server policy decision, never a client-controlled value.
func stageGPT61SolChatClientSession(c *gin.Context, account *Account, body []byte, actualModel string, enabled bool) func() {
	if !enabled || c == nil || c.Request == nil || !isCodexOAuthAccount(account) ||
		account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() ||
		!openai.IsGPT61SolModelSpelling(actualModel) || !gjson.GetBytes(body, "messages").IsArray() {
		return stageCodexHTTPClientSession(c, account, body)
	}
	value, _ := c.Get("api_key")
	key, ok := value.(*APIKey)
	if !ok || key == nil || key.ID <= 0 || key.UserID <= 0 || codexAccountIdentityNamespace(codexAccountIdentitySource(c, account)) == "" {
		return stageCodexHTTPClientSession(c, account, body)
	}
	resolved := resolveGPT61SolChatIdentity(c, body)
	original := c.Request.Header
	headers := original.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	ev := resolved.evidence
	if !resolved.valid || (ev.ClientSessionID == "" && ev.ClientThreadID == "" && ev.ClientTurnID == "") {
		for _, name := range gpt61SolChatIdentityHeaders() {
			headers.Del(name)
		}
		headers.Del(openAIWSTurnMetadataHeader)
		session := codexAnonymousRequestSession(c)
		headers.Set("session-id", session)
		c.Set("codex_http_session_scope", session+"\x00"+session)
	} else {
		// Only allowlisted identity fields cross the typed Chat conversion. Keep
		// unrelated existing header metadata intact; do not copy body extras.
		updates := map[string]any{}
		for name, value := range map[string]string{
			"session_id": ev.ClientSessionID, "thread_id": ev.ClientThreadID, "turn_id": ev.ClientTurnID,
			"parent_thread_id": ev.ParentThreadID, "forked_from_thread_id": ev.ForkedThreadID,
			"root_turn_id": ev.RootTurnID, "parent_turn_id": ev.ParentTurnID,
		} {
			if value != "" {
				updates[name] = value
			}
		}
		raw := headers.Get(openAIWSTurnMetadataHeader)
		if strings.TrimSpace(raw) == "" {
			raw = "{}"
		}
		if ev.ClientTurnStartedAtMs > 0 {
			updates["turn_started_at_unix_ms"] = ev.ClientTurnStartedAtMs
		} else {
			// A lower-priority header timestamp belongs to its original turn, not
			// to a body turn that took precedence without supplying a timestamp.
			raw, _ = sjson.Delete(raw, "turn_started_at_unix_ms")
		}
		raw = rewriteCodexTurnMetadataJSON(raw, false, func(map[string]any) map[string]any { return updates })
		if !json.Valid([]byte(raw)) {
			return stageCodexHTTPClientSession(c, account, body)
		}
		headers.Set(openAIWSTurnMetadataHeader, raw)
		if ev.ClientSessionID != "" {
			headers.Set("session-id", ev.ClientSessionID)
		}
		if ev.ClientThreadID != "" {
			headers.Set("thread-id", ev.ClientThreadID)
		}
		if ev.ClientTurnID != "" {
			headers.Set("turn-id", ev.ClientTurnID)
		}
		if session := firstNonEmptyString(ev.ClientSessionID, ev.ClientThreadID); session != "" {
			thread := firstNonEmptyString(ev.ClientThreadID, session)
			c.Set("codex_http_session_scope", session+"\x00"+thread)
		} else {
			c.Set("codex_http_session_scope", "turn:"+ev.ClientTurnID)
		}
	}
	c.Request.Header = headers
	return func() { c.Request.Header = original }
}
