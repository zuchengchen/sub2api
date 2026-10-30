package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const codexAccountIdentityNamespaceVersion = "v1"

const codexAccountIdentitySourceContextKey = "openai_codex_account_identity_source"

// prepareCodexAccountIdentitySource stages the selected account for each attempt.
// The handler reuses gin.Context across failover attempts, so every entry
// point overwrites the staged source before projecting outbound identity.
func (s *OpenAIGatewayService) prepareCodexAccountIdentitySource(ctx context.Context, c *gin.Context, account *Account) (*Account, error) {
	if c != nil {
		c.Set(codexAccountIdentitySourceContextKey, account)
	}
	return account, nil
}

func codexAccountIdentitySource(c *gin.Context, fallback *Account) *Account {
	if c != nil {
		if staged, ok := c.Get(codexAccountIdentitySourceContextKey); ok {
			if source, ok := staged.(*Account); ok && source != nil {
				return source
			}
		}
	}
	return fallback
}

// codexAccountIdentityNamespace returns a stable, credential-scoped namespace.
// Multiple local rows that use the same ChatGPT account intentionally share the
// same namespace. Setup tokens use an irreversible bearer fingerprint because
// they have no refresh lifecycle or imported account metadata. Refreshable OAuth
// otherwise falls back only to a persistent fingerprint seed: local row IDs are
// deployment-relative and must never become upstream identity.
func codexAccountIdentityNamespace(account *Account) string {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return ""
	}
	if upstreamAccountID := strings.TrimSpace(account.GetChatGPTAccountID()); upstreamAccountID != "" {
		if upstreamUserID := strings.TrimSpace(account.GetCredential("chatgpt_user_id")); upstreamUserID != "" {
			return "chatgpt:" + upstreamAccountID + ":user:" + upstreamUserID
		}
		return "chatgpt:" + upstreamAccountID
	}
	if seed, ok := codexFingerprintSeed(account.Extra); ok {
		return "seed:" + seed
	}
	if account.Type == AccountTypeSetupToken {
		if token := strings.TrimSpace(account.GetOpenAIAccessToken()); token != "" {
			sum := sha256.Sum256([]byte("openai-setup-token:" + token))
			return fmt.Sprintf("setup-token:%x", sum[:16])
		}
	}
	return ""
}

// isolateOpenAIUpstreamSessionID preserves the existing API-key isolation while
// adding the selected OAuth credential namespace. A scheduler failover therefore
// cannot send the same session/conversation identity through two upstream accounts.
func isolateOpenAIUpstreamSessionID(apiKeyID int64, account *Account, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	namespace := codexAccountIdentityNamespace(account)
	if namespace == "" {
		return isolateOpenAISessionID(apiKeyID, raw)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("u%d:a%s:%s", apiKeyID, namespace, raw)))
	return fmt.Sprintf("%x", sum[:8])
}

func scopeCodexAccountIdentityValue(account *Account, apiKeyID int64, kind, raw string) string {
	raw = strings.TrimSpace(raw)
	namespace := codexAccountIdentityNamespace(account)
	if raw == "" || namespace == "" {
		return raw
	}
	if derived, ok := deriveCodexIdentityCompositeValue(account, apiKeyID, kind, raw); ok {
		return derived
	}
	seed := fmt.Sprintf(
		"sub2api:codex-account-identity:%s:user:%d:account:%s:kind:%s:value:%s",
		codexAccountIdentityNamespaceVersion,
		apiKeyID,
		namespace,
		codexIdentitySeedKind(kind),
		raw,
	)
	// Seeded session/full accounts use one mapping for primary IDs and all
	// references to them. Keep unseeded accounts on their existing mapping.
	if _, ok := codexFingerprintSeed(account.Extra); ok &&
		(account.GetCodexFingerprintMode() == codexFingerprintFull || account.GetCodexFingerprintMode() == codexFingerprintSession) &&
		(codexIdentitySeedKind(kind) == "thread" || kind == "turn") {
		created := codexUUIDv7Millis(raw)
		if created <= 0 {
			created = defaultCodexTurnClock.StartedAt(seed)
		}
		return deriveStableCodexUUIDv7(seed, created)
	}
	if derived, ok := deriveCodexConvergenceIdentityValue(account, seed, raw); ok {
		return derived
	}
	return deriveStableUUIDv4(seed)
}

var codexAccountIdentityFields = []struct {
	name string
	kind string
}{
	{name: "installation_id", kind: "installation"},
	{name: "x-codex-installation-id", kind: "installation"},
	{name: "session_id", kind: "session"},
	{name: "session-id", kind: "session"},
	{name: "thread_id", kind: "thread"},
	{name: "thread-id", kind: "thread"},
	{name: "turn_id", kind: "turn"},
	{name: "turn-id", kind: "turn"},
	{name: "window_id", kind: "window"},
	{name: "x-codex-window-id", kind: "window"},
	{name: "x-client-request-id", kind: "request"},
}

func applyCodexAccountIdentityFields(values map[string]any, account *Account, apiKeyID int64) bool {
	if values == nil || codexAccountIdentityNamespace(account) == "" {
		return false
	}
	changed := false
	for _, field := range codexAccountIdentityFields {
		raw, ok := values[field.name].(string)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		next := scopeCodexAccountIdentityValue(account, apiKeyID, field.kind, raw)
		if next != raw {
			values[field.name] = next
			changed = true
		}
	}
	if applyCodexConvergenceIdentityFields(values, account, apiKeyID) {
		changed = true
	}
	return changed
}

func applyCodexAccountIdentityEmbeddedMetadata(values map[string]any, account *Account, apiKeyID int64) bool {
	raw, ok := values[openAIWSTurnMetadataHeader].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return false
	}
	next := scopeCodexAccountTurnMetadata(raw, account, apiKeyID)
	if next == raw {
		return false
	}
	values[openAIWSTurnMetadataHeader] = next
	return true
}

func scopeCodexAccountTurnMetadata(raw string, account *Account, apiKeyID int64) string {
	return rewriteCodexTurnMetadataJSON(raw, false, func(metadata map[string]any) map[string]any {
		before := maps.Clone(metadata)
		if !applyCodexAccountIdentityFields(metadata, account, apiKeyID) {
			return nil
		}
		fields := make(map[string]any)
		for name, value := range metadata {
			if text, ok := value.(string); ok && text != before[name] {
				fields[name] = text
			}
		}
		return fields
	})
}

func applyCodexAccountIdentityClientMetadataMap(requestBody map[string]any, account *Account, apiKeyID int64) bool {
	if requestBody == nil || codexAccountIdentityNamespace(account) == "" {
		return false
	}
	changed := false
	clientMetadata, _ := requestBody["client_metadata"].(map[string]any)
	originalBodySessionID := ""
	if clientMetadata != nil {
		originalBodySessionID, _ = clientMetadata["session_id"].(string)
		if applyCodexAccountIdentityFields(clientMetadata, account, apiKeyID) {
			changed = true
		}
		if applyCodexAccountIdentityEmbeddedMetadata(clientMetadata, account, apiKeyID) {
			changed = true
		}
	}
	if raw, ok := requestBody["prompt_cache_key"].(string); ok && strings.TrimSpace(raw) != "" {
		kind := "prompt-cache"
		if strings.TrimSpace(originalBodySessionID) != "" && raw == originalBodySessionID {
			kind = "session"
		}
		next := scopeCodexAccountIdentityValue(account, apiKeyID, kind, raw)
		if next != raw {
			requestBody["prompt_cache_key"] = next
			changed = true
		}
	}
	if isCodexOAuthAccount(account) {
		input, _ := requestBody["input"].([]any)
		turns := make(map[string]string)
		for _, value := range input {
			item, _ := value.(map[string]any)
			metadata, _ := item["internal_chat_message_metadata_passthrough"].(map[string]any)
			raw, _ := metadata["turn_id"].(string)
			if strings.TrimSpace(raw) == "" {
				continue
			}
			scoped, ok := turns[raw]
			if !ok {
				scoped = scopeCodexAccountIdentityValue(account, apiKeyID, "turn", raw)
				turns[raw] = scoped
			}
			if scoped != raw {
				metadata["turn_id"] = scoped
				changed = true
			}
		}
	}
	return changed
}

// applyCodexAccountIdentityClientMetadataRaw scopes only the small identity
// subobjects and input turn references. All replacements use original offsets
// and one output allocation, preserving user content and input key order without
// repeatedly copying a potentially multi-megabyte conversation.
func applyCodexAccountIdentityClientMetadataRaw(body []byte, account *Account, apiKeyID int64) ([]byte, bool, error) {
	if len(body) == 0 || codexAccountIdentityNamespace(account) == "" {
		return body, false, nil
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body, false, nil
	}

	type edit struct {
		start, end int
		value      []byte
	}
	var edits []edit
	add := func(value gjson.Result, replacement []byte) {
		edits = append(edits, edit{value.Index, value.Index + len(value.Raw), replacement})
	}
	originalBodySessionID := ""
	if cm := root.Get("client_metadata"); cm.IsObject() {
		clientMetadata := map[string]any{}
		if err := decodeCodexMetadataObject(cm.Raw, &clientMetadata); err != nil {
			return body, false, fmt.Errorf("decode client_metadata for account identity: %w", err)
		}
		originalBodySessionID, _ = clientMetadata["session_id"].(string)
		metadataChanged := applyCodexAccountIdentityFields(clientMetadata, account, apiKeyID)
		if applyCodexAccountIdentityEmbeddedMetadata(clientMetadata, account, apiKeyID) {
			metadataChanged = true
		}
		if metadataChanged {
			raw, err := json.Marshal(clientMetadata)
			if err != nil {
				return body, false, fmt.Errorf("encode account-scoped client_metadata: %w", err)
			}
			add(cm, raw)
		}
	}
	if promptCacheKey := root.Get("prompt_cache_key"); promptCacheKey.Type == gjson.String && strings.TrimSpace(promptCacheKey.String()) != "" {
		raw := promptCacheKey.String()
		kind := "prompt-cache"
		if strings.TrimSpace(originalBodySessionID) != "" && raw == originalBodySessionID {
			kind = "session"
		}
		scoped := scopeCodexAccountIdentityValue(account, apiKeyID, kind, raw)
		if scoped != raw {
			add(promptCacheKey, []byte(strconv.Quote(scoped)))
		}
	}
	if isCodexOAuthAccount(account) {
		turns := make(map[string]string)
		if input := root.Get("input"); input.IsArray() {
			input.ForEach(func(_, item gjson.Result) bool {
				if !item.IsObject() {
					return true
				}
				turn := item.Get("internal_chat_message_metadata_passthrough.turn_id")
				if turn.Type != gjson.String || strings.TrimSpace(turn.Str) == "" {
					return true
				}
				scoped, ok := turns[turn.Str]
				if !ok {
					scoped = scopeCodexAccountIdentityValue(account, apiKeyID, "turn", turn.Str)
					turns[turn.Str] = scoped
				}
				if scoped != turn.Str {
					add(turn, []byte(strconv.Quote(scoped)))
				}
				return true
			})
		}
	}
	if len(edits) == 0 {
		return body, false, nil
	}
	slices.SortFunc(edits, func(a, b edit) int { return a.start - b.start })
	size, end := len(body), 0
	for _, e := range edits {
		if e.start < end || e.end < e.start || e.end > len(body) {
			return body, false, fmt.Errorf("invalid account identity replacement bounds")
		}
		size += len(e.value) - (e.end - e.start)
		end = e.end
	}
	next := make([]byte, 0, size)
	end = 0
	for _, e := range edits {
		next = append(next, body[end:e.start]...)
		next = append(next, e.value...)
		end = e.end
	}
	next = append(next, body[end:]...)
	return next, true, nil
}

func applyCodexAccountIdentityHeaders(headers http.Header, account *Account, apiKeyID int64) {
	if headers == nil || codexAccountIdentityNamespace(account) == "" {
		return
	}
	for _, field := range codexAccountIdentityFields {
		// Underscore session/conversation headers are rebuilt separately from the
		// prompt cache key by each request builder.
		if field.name == "session_id" {
			continue
		}
		raw := strings.TrimSpace(headers.Get(field.name))
		if raw != "" {
			headers.Set(field.name, scopeCodexAccountIdentityValue(account, apiKeyID, field.kind, raw))
		}
	}
	if codexFingerprintConvergenceEnabled(account) {
		if parent := strings.TrimSpace(headers.Get("x-codex-parent-thread-id")); parent != "" {
			headers.Set("x-codex-parent-thread-id", scopeCodexAccountIdentityValue(account, apiKeyID, "thread", parent))
		}
	}
	if raw := headers.Get(openAIWSTurnMetadataHeader); strings.TrimSpace(raw) != "" {
		headers.Set(openAIWSTurnMetadataHeader, scopeCodexAccountTurnMetadata(raw, account, apiKeyID))
	}
}
