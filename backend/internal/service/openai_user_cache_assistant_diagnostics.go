package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	userCacheAssistantMaxMessage   = 256 << 10
	userCacheAssistantMaxTailBytes = 1 << 20
	userCacheAssistantMaxBody      = 4 << 20
	userCacheAssistantMaxSummary   = 16 << 10
	userCacheAssistantTailCount    = 4
)

var userCacheAssistantFields = [...]string{
	"role", "content", "reasoning_content", "reasoning", "refusal", "name", "tool_calls", "tool_call_id", "function_call",
}

// A failed entropy read stays unavailable for this process. There is no raw-SHA
// or predictable-key fallback. Tests use an independent instance, never reset
// the live key or alter crypto/rand.Reader.
type userCacheAssistantHasher struct {
	once    sync.Once
	entropy io.Reader
	key     [32]byte
	ready   bool
}

var userCacheAssistantProcessHasher = userCacheAssistantHasher{entropy: rand.Reader}

func (h *userCacheAssistantHasher) digest(userID, keyID int64, purpose string, raw []byte) (string, bool) {
	if h == nil || userID <= 0 || keyID <= 0 {
		return "", false
	}
	h.once.Do(func() {
		if h.entropy != nil {
			_, err := io.ReadFull(h.entropy, h.key[:])
			h.ready = err == nil
		}
		if !h.ready {
			clear(h.key[:])
		}
	})
	if !h.ready {
		return "", false
	}
	mac := hmac.New(sha256.New, h.key[:])
	for _, field := range [][]byte{[]byte("user-cache-assistant/v1"), []byte(strconv.FormatInt(userID, 10)), []byte(strconv.FormatInt(keyID, 10)), []byte(purpose), raw} {
		nativeReplayHashField(mac, field)
	}
	return hex.EncodeToString(mac.Sum(nil)), true
}

type userCacheAssistantDigest struct {
	Valid bool   `json:"valid"`
	Bytes int    `json:"bytes"`
	HMAC  string `json:"hmac_sha256,omitempty"`
}

type userCacheAssistantField struct {
	Present        bool   `json:"present"`
	Type           string `json:"type"`
	RawBytes       int    `json:"raw_bytes"`
	CanonicalBytes int    `json:"canonical_bytes"`
	HMAC           string `json:"hmac_sha256,omitempty"`
	Empty          *bool  `json:"empty,omitempty"`
	Count          *int   `json:"count,omitempty"`
}

type userCacheAssistantUnknown struct {
	Count int    `json:"count"`
	Bytes int    `json:"bytes"`
	HMAC  string `json:"hmac_sha256"`
}

type userCacheAssistantComparison struct {
	Schema    int                                `json:"schema"`
	Available bool                               `json:"available"`
	Reason    string                             `json:"unavailable_reason,omitempty"`
	RawBytes  int                                `json:"raw_bytes"`
	Object    userCacheAssistantDigest           `json:"object_canonical"`
	Native    userCacheAssistantDigest           `json:"native_canonical"`
	Fields    map[string]userCacheAssistantField `json:"fields,omitempty"`
	Unknown   *userCacheAssistantUnknown         `json:"unknown_fields,omitempty"`
}

type userCacheAssistantTailItem struct {
	Index      int                          `json:"index"`
	Value      userCacheValueSummary        `json:"value"`
	Comparison userCacheAssistantComparison `json:"comparison"`
}

type userCacheAssistantTail struct {
	Schema            int                          `json:"schema"`
	Available         bool                         `json:"available"`
	Reason            string                       `json:"unavailable_reason,omitempty"`
	MessageCount      int                          `json:"message_count"`
	AssistantCount    int                          `json:"assistant_count"`
	OmittedAssistants int                          `json:"omitted_assistants"`
	Items             []userCacheAssistantTailItem `json:"items,omitempty"`
}

func unavailableUserCacheAssistant(reason string) userCacheAssistantComparison {
	return userCacheAssistantComparison{Schema: 1, Reason: reason}
}

// Called only after admission, and for delivery only after a successful complete
// capture. The caller must release capture locks before computing this summary.
// No mutable request, replay engine, Redis state, or plaintext cache is involved.
func (d *userCacheDiagnostic) summarizeAssistant(raw []byte) userCacheAssistantComparison {
	return d.summarizeAssistantWithHasher(raw, &userCacheAssistantProcessHasher)
}

func (d *userCacheDiagnostic) summarizeAssistantWithHasher(raw []byte, hasher *userCacheAssistantHasher) userCacheAssistantComparison {
	if d == nil || d.config == nil || d.config.userID <= 0 || d.apiKeyID <= 0 {
		return unavailableUserCacheAssistant("invalid_scope")
	}
	if len(raw) > userCacheAssistantMaxMessage {
		return unavailableUserCacheAssistant("message_limit")
	}
	if !utf8.Valid(raw) {
		return unavailableUserCacheAssistant("invalid_json")
	}
	canonical, err := nativeReplayCanonicalObject(raw)
	if err != nil {
		return unavailableUserCacheAssistant("invalid_json")
	}
	if len(canonical) > userCacheAssistantMaxMessage {
		return unavailableUserCacheAssistant("canonical_limit")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return unavailableUserCacheAssistant("invalid_json")
	}
	var role string
	if json.Unmarshal(object["role"], &role) != nil || role != "assistant" {
		return unavailableUserCacheAssistant("not_assistant")
	}
	hash := func(purpose string, value []byte) string {
		result, _ := hasher.digest(d.config.userID, d.apiKeyID, purpose, value)
		return result
	}
	objectHash := hash("object", canonical)
	if objectHash == "" {
		return unavailableUserCacheAssistant("entropy_unavailable")
	}
	out := userCacheAssistantComparison{Schema: 1, Available: true, RawBytes: len(raw), Object: userCacheAssistantDigest{Valid: true, Bytes: len(canonical), HMAC: objectHash}, Fields: make(map[string]userCacheAssistantField, len(userCacheAssistantFields))}
	for _, name := range userCacheAssistantFields {
		value, present := object[name]
		field := userCacheAssistantField{Present: present, Type: "missing"}
		if present {
			encoded, err := nativeReplayCanonical(value)
			if err != nil {
				return unavailableUserCacheAssistant("invalid_json")
			}
			field.RawBytes, field.CanonicalBytes, field.HMAC = len(value), len(encoded), hash("field/"+name, encoded)
			switch encoded[0] {
			case 'n':
				field.Type = "null"
			case '"':
				field.Type = "string"
				var text string
				_ = json.Unmarshal(encoded, &text)
				empty := text == ""
				field.Empty = &empty
			case '[':
				field.Type = "array"
				var values []json.RawMessage
				_ = json.Unmarshal(encoded, &values)
				count := len(values)
				field.Count = &count
			case '{':
				field.Type = "object"
			case 't', 'f':
				field.Type = "boolean"
			default:
				field.Type = "number"
			}
		}
		out.Fields[name] = field
		delete(object, name)
	}
	unknown, err := json.Marshal(object)
	if err != nil {
		return unavailableUserCacheAssistant("invalid_json")
	}
	unknown, err = nativeReplayCanonicalObject(unknown)
	if err != nil {
		return unavailableUserCacheAssistant("invalid_json")
	}
	out.Unknown = &userCacheAssistantUnknown{Count: len(object), Bytes: len(unknown), HMAC: hash("unknown", unknown)}
	// The exact production normalizer preserves unknown fields and nonempty
	// reasoning. Its result is evidence, never an alternate matching policy.
	if native, err := nativeReplayCanonicalMessage(raw); err == nil {
		if len(native) > userCacheAssistantMaxMessage {
			return unavailableUserCacheAssistant("canonical_limit")
		}
		out.Native = userCacheAssistantDigest{Valid: true, Bytes: len(native), HMAC: hash("native", native)}
	}
	if encoded, err := json.Marshal(out); err != nil || len(encoded) > userCacheAssistantMaxSummary {
		return unavailableUserCacheAssistant("summary_limit")
	}
	return out
}

func (d *userCacheDiagnostic) summarizeAssistantTail(body []byte) userCacheAssistantTail {
	return d.summarizeAssistantTailWithHasher(body, &userCacheAssistantProcessHasher)
}

func (d *userCacheDiagnostic) summarizeAssistantTailWithHasher(body []byte, hasher *userCacheAssistantHasher) userCacheAssistantTail {
	out := userCacheAssistantTail{Schema: 1}
	fail := func(reason string) userCacheAssistantTail {
		out.Available, out.Reason, out.Items = false, reason, nil
		return out
	}
	if d == nil || d.config == nil || d.config.userID <= 0 || d.apiKeyID <= 0 {
		return fail("invalid_scope")
	}
	if len(body) > userCacheAssistantMaxBody {
		return fail("body_limit")
	}
	if !utf8.Valid(body) {
		return fail("invalid_json")
	}
	// Validate the entire document: extracting a tail must not hide duplicate
	// root/nested members, excessive depth, or ambiguous messages arrays.
	canonical, err := nativeReplayCanonicalObject(body)
	if err != nil {
		return fail("invalid_json")
	}
	if len(canonical) > userCacheAssistantMaxBody {
		return fail("body_limit")
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(body, &root)
	for name := range root {
		// The typed request decoder accepts case-insensitive field names, while
		// diagnostic paths are exact. Do not select one ambiguous carrier.
		if name != "messages" && strings.EqualFold(name, "messages") {
			return fail("ambiguous_messages")
		}
	}
	var messages []json.RawMessage
	if len(root["messages"]) == 0 || root["messages"][0] != '[' || json.Unmarshal(root["messages"], &messages) != nil {
		return fail("invalid_messages")
	}
	out.MessageCount = len(messages)
	if len(messages) > userCacheDiagnosticMaxItems {
		return fail("history_summary_truncated")
	}
	indexes := make([]int, 0, userCacheAssistantTailCount)
	for index, raw := range messages {
		var message struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &message) != nil || message.Role != "assistant" {
			continue
		}
		out.AssistantCount++
		if len(indexes) == userCacheAssistantTailCount {
			copy(indexes, indexes[1:])
			indexes = indexes[:userCacheAssistantTailCount-1]
		}
		indexes = append(indexes, index)
	}
	out.OmittedAssistants = out.AssistantCount - len(indexes)
	bytes := 0
	for _, index := range indexes {
		raw := messages[index]
		if len(raw) > userCacheAssistantMaxTailBytes-bytes {
			return fail("tail_limit")
		}
		bytes += len(raw)
		comparison := d.summarizeAssistantWithHasher(raw, hasher)
		if !comparison.Available {
			return fail(comparison.Reason)
		}
		out.Items = append(out.Items, userCacheAssistantTailItem{Index: index, Value: summarizeUserCacheValue(raw), Comparison: comparison})
	}
	out.Available = true
	if encoded, err := json.Marshal(out); err != nil || len(encoded) > userCacheAssistantMaxSummary {
		return fail("summary_limit")
	}
	return out
}
