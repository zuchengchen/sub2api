package service

import (
	"maps"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// Raw field and item HMACs preserve wire differences. A second HMAC for
// bounded, unambiguous JSON ignores formatting and object member order.
const (
	responsesDiagnosticPrefixBytes    = 4 << 20
	responsesDiagnosticCanonicalBytes = 256 << 10
)

type responsesCacheDiagnosticValue struct {
	Bytes         int    `json:"bytes"`
	HMAC          string `json:"hmac,omitempty"`
	CanonicalHMAC string `json:"canonical_hmac,omitempty"`
	State         string `json:"state,omitempty"`
}

type responsesCacheDiagnosticItem struct {
	responsesCacheDiagnosticValue
	Role string `json:"role,omitempty"`
	Type string `json:"type,omitempty"`
}

type responsesCacheDiagnosticInput struct {
	Kind      string                         `json:"kind"`
	Items     []responsesCacheDiagnosticItem `json:"items,omitempty"`
	Truncated bool                           `json:"truncated"`
}

type responsesCacheDiagnosticFields map[string]gjson.Result

type responsesCacheDiagnosticPayload struct {
	body, envelope responsesCacheDiagnosticFields
}

// Read a large top-level history once. The typed HTTP decoder and WS parser
// can disagree on duplicate and case-variant keys, so ambiguous requests
// cannot spend quota or provide identity evidence.
func responsesCacheDiagnosticObject(object gjson.Result) responsesCacheDiagnosticFields {
	if !object.IsObject() {
		return nil
	}
	fields := make(responsesCacheDiagnosticFields)
	object.ForEach(func(key, value gjson.Result) bool {
		if len(fields) >= 128 || len(key.Str) > 256 {
			fields = nil
			return false
		}
		folded := strings.ToLower(key.Str)
		if _, duplicate := fields[folded]; duplicate {
			fields = nil
			return false
		}
		if key.Str != folded {
			switch folded {
			case "model", "response", "type", "id", "input", "generate", "instructions", "tools", "tool_choice", "parallel_tool_calls",
				"prompt", "reasoning", "text", "truncation", "client_metadata", "metadata", "conversation", "prompt_cache_key",
				"previous_response_id", "session_id", "conversation_id", "thread_id", "parent_thread_id", "x-codex-parent-thread-id",
				"forked_from_thread_id", "turn_id", "root_turn_id", "parent_turn_id":
				fields = nil
				return false
			}
			value = gjson.Result{} // reserve the name to reject a later ambiguous duplicate
		}
		fields[folded] = value
		return true
	})
	return fields
}

func parseResponsesCacheDiagnosticPayload(body []byte, transport string) (responsesCacheDiagnosticPayload, string, bool) {
	var empty responsesCacheDiagnosticPayload
	if len(body) == 0 || len(body) > userCacheDiagnosticMaxBytes || !utf8.Valid(body) || !gjson.ValidBytes(body) {
		return empty, "", false
	}
	payload := responsesCacheDiagnosticPayload{body: responsesCacheDiagnosticObject(parseRawJSONView(body))}
	if payload.body == nil || payload.body["generate"].Type == gjson.False {
		return empty, "", false
	}
	if transport == "ws" {
		event := payload.body["type"]
		if event.Exists() && (event.Type != gjson.String || event.Str != "response.create") {
			return empty, "", false
		}
		if inner := payload.body["response"]; inner.Exists() {
			if payload.body["model"].Exists() || payload.body["input"].Exists() {
				return empty, "", false
			}
			payload.envelope, payload.body = payload.body, responsesCacheDiagnosticObject(inner)
			if payload.body == nil {
				return empty, "", false
			}
		}
	}
	source := "body"
	if value := payload.body["model"]; value.Exists() {
		if value.Type != gjson.String || len(value.Str) > 256 || strings.TrimSpace(value.Str) == "" {
			return empty, "", false
		}
	} else if transport == "ws" {
		source = "ws_inherited"
	} else {
		return empty, "", false
	}
	choice := payload.body["tool_choice"]
	if payload.body["generate"].Type == gjson.False || len(choice.Raw) > gpt61SolChatIdentityMaxMetadataBytes ||
		openAIJSONToolChoiceSelectsExplicitImageGeneration(choice) || payload.body["input"].Get(`#(type=="compaction_trigger").type`).Exists() {
		return empty, "", false
	}
	return payload, source, true
}

func (w *responsesCacheDiagnosticWindow) prefix(payload responsesCacheDiagnosticFields) (responsesCacheDiagnosticInput, map[string]responsesCacheDiagnosticValue) {
	fields := make(map[string]responsesCacheDiagnosticValue)
	remaining := responsesDiagnosticPrefixBytes
	for _, name := range []string{"instructions", "tools", "tool_choice", "parallel_tool_calls", "prompt", "reasoning", "text", "truncation"} {
		if value := payload[name]; value.Exists() {
			fields[name] = w.jsonFingerprint(name, value, &remaining)
		}
	}
	input := payload["input"]
	out := responsesCacheDiagnosticInput{Kind: "absent"}
	add := func(item gjson.Result) bool {
		if len(out.Items) >= userCacheDiagnosticMaxItems || remaining == 0 {
			out.Truncated = true
			return false
		}
		value := w.jsonFingerprint("input_item", item, &remaining)
		entry := responsesCacheDiagnosticItem{responsesCacheDiagnosticValue: value}
		if value.State == "limit" {
			out.Truncated = true
		}
		if item.IsObject() && value.HMAC != "" {
			switch role := item.Get("role").Str; role {
			case "user", "assistant", "system", "developer", "tool", "function":
				entry.Role = strings.Clone(role)
			}
			if kind := item.Get("type"); kind.Exists() {
				switch kind.Str {
				case "message", "reasoning", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output",
					"tool_search_call", "tool_search_output", "web_search_call", "image_generation_call", "item_reference",
					"compaction", "compaction_summary", "configuration_update", "additional_tools":
					entry.Type = strings.Clone(kind.Str)
				default:
					entry.Type = "other"
				}
			}
		}
		out.Items = append(out.Items, entry)
		return true
	}
	switch {
	case input.IsArray():
		out.Kind = "array"
		input.ForEach(func(_, item gjson.Result) bool { return add(item) })
	case input.Type == gjson.String:
		out.Kind = "string"
		add(input)
	case input.Exists():
		out.Kind = "other"
	}
	return out, fields
}

func (w *responsesCacheDiagnosticWindow) jsonFingerprint(purpose string, value gjson.Result, remaining *int) responsesCacheDiagnosticValue {
	size := len(value.Raw)
	out := responsesCacheDiagnosticValue{Bytes: size}
	if size > *remaining {
		out.State = "limit"
		return out
	}
	*remaining -= size
	raw := []byte(value.Raw)
	out.HMAC = w.mac("raw:"+purpose, raw)
	if size > responsesDiagnosticCanonicalBytes {
		out.State = "canonical_limit"
		return out
	}
	canonical, err := nativeReplayCanonical(raw)
	if err != nil {
		out.State = "canonical_unavailable"
		return out
	}
	out.CanonicalHMAC = w.mac("json:"+purpose, canonical)
	return out
}

func (w *responsesCacheDiagnosticWindow) identityValue(purpose, value string) responsesCacheDiagnosticValue {
	summary := responsesCacheDiagnosticValue{Bytes: len(value)}
	if len(value) > 4*maxPersistedSessionIDLength {
		summary.State = "limit"
		return summary
	}
	if purpose == "response_id" && openAIResponseIDPattern.MatchString(strings.TrimSpace(value)) {
		// A valid response identifier can be a few bytes longer than a
		// persisted session ID. It has a dedicated protocol grammar.
		value = strings.TrimSpace(value)
	} else {
		value = sanitizeGPT61SolChatIdentity(value)
		if value == "" {
			summary.State = "invalid"
			return summary
		}
	}
	summary.HMAC = w.mac(purpose, []byte(value))
	return summary
}

func (w *responsesCacheDiagnosticWindow) headerIdentity(headers http.Header) map[string]responsesCacheDiagnosticValue {
	out := make(map[string]responsesCacheDiagnosticValue)
	add := func(name, purpose string) {
		values := headers.Values(name)
		source := "header:" + strings.ToLower(name)
		switch len(values) {
		case 0:
		case 1:
			out[source] = w.identityValue(purpose, values[0])
		default:
			out[source] = responsesCacheDiagnosticValue{State: "ambiguous"}
		}
	}
	for _, name := range clientSessionIDHeaders {
		if strings.EqualFold(name, claudeCodeSessionHeader) {
			continue
		}
		add(name, "session")
	}
	if values := headers.Values(claudeCodeSessionHeader); len(values) != 0 {
		source := "header:" + strings.ToLower(claudeCodeSessionHeader)
		if session, ok := validatedResponsesClaudeHeader(values); ok && session != "" {
			out[source] = w.identityValue("session", session)
		} else {
			out[source] = responsesCacheDiagnosticValue{State: "invalid"}
		}
	}
	add("thread-id", "thread")
	add("turn-id", "turn")
	add("x-codex-parent-thread-id", "thread")
	switch state := headers.Values(openAIWSTurnStateHeader); len(state) {
	case 0:
	case 1:
		out["header:"+openAIWSTurnStateHeader] = w.turnStateValue(state[0])
	default:
		out["header:"+openAIWSTurnStateHeader] = responsesCacheDiagnosticValue{State: "ambiguous"}
	}
	values := headers.Values(openAIWSTurnMetadataHeader)
	if len(values) == 1 {
		w.metadataIdentity(out, "header:turn_metadata", values[0])
	} else if len(values) > 1 {
		out["header:turn_metadata"] = responsesCacheDiagnosticValue{State: "ambiguous"}
	}
	return out
}

func (w *responsesCacheDiagnosticWindow) turnStateValue(raw string) responsesCacheDiagnosticValue {
	out := responsesCacheDiagnosticValue{Bytes: len(raw)}
	if len(raw) > gpt61SolChatIdentityMaxMetadataBytes {
		out.State = "limit"
		return out
	}
	value := strings.TrimSpace(raw)
	if value == "" {
		out.State = "invalid"
		return out
	}
	// A turn-state token is opaque and can exceed persisted session ID limits.
	// Give it a separate purpose so it never compares as a session or thread.
	out.HMAC = w.mac("turn_state", []byte(value))
	return out
}

func (w *responsesCacheDiagnosticWindow) addIdentity(out map[string]responsesCacheDiagnosticValue, source, purpose string, value gjson.Result) {
	if value.Type == gjson.String {
		out[source] = w.identityValue(purpose, value.Str)
	} else if value.Exists() {
		out[source] = responsesCacheDiagnosticValue{Bytes: len(value.Raw), State: "invalid"}
	}
}

// Emit only fixed, recognized carriers. An affinity hint or matching content
// is evidence about routing, never a generated conversation identity.
func (w *responsesCacheDiagnosticWindow) identityFields(out map[string]responsesCacheDiagnosticValue, source string, get func(string) gjson.Result) {
	for _, field := range []struct{ name, purpose string }{
		{"session_id", "session"}, {"conversation_id", "session"}, {"thread_id", "thread"},
		{"parent_thread_id", "thread"}, {"x-codex-parent-thread-id", "thread"}, {"forked_from_thread_id", "thread"},
		{"turn_id", "turn"}, {"root_turn_id", "turn"}, {"parent_turn_id", "turn"},
	} {
		w.addIdentity(out, source+":"+field.name, field.purpose, get(field.name))
	}
}

func (w *responsesCacheDiagnosticWindow) metadataIdentity(out map[string]responsesCacheDiagnosticValue, source, raw string) gjson.Result {
	if len(raw) > gpt61SolChatIdentityMaxMetadataBytes {
		out[source] = responsesCacheDiagnosticValue{Bytes: len(raw), State: "limit"}
		return gjson.Result{}
	}
	meta, ok := gpt61SolChatIdentityMetadata(raw)
	if !ok {
		out[source] = responsesCacheDiagnosticValue{Bytes: len(raw), State: "invalid"}
		return gjson.Result{}
	}
	w.identityFields(out, source, meta.Get)
	return meta
}

func (w *responsesCacheDiagnosticWindow) inputIdentity(headers map[string]responsesCacheDiagnosticValue, payload responsesCacheDiagnosticPayload) map[string]responsesCacheDiagnosticValue {
	out := maps.Clone(headers)
	w.appendBodyIdentity(out, "body", payload.body)
	if payload.envelope != nil {
		w.appendBodyIdentity(out, "envelope", payload.envelope)
	}
	return out
}

func (w *responsesCacheDiagnosticWindow) appendBodyIdentity(out map[string]responsesCacheDiagnosticValue, prefix string, fields responsesCacheDiagnosticFields) {
	w.identityFields(out, prefix, func(key string) gjson.Result { return fields[key] })
	for _, name := range []string{"client_metadata", "metadata"} {
		if raw := fields[name]; raw.Exists() {
			source := prefix + ":" + name
			meta := w.metadataIdentity(out, source, raw.Raw)
			if state := meta.Get(openAIWSTurnStateHeader); state.Exists() && name == "client_metadata" {
				if state.Type == gjson.String {
					out[source+":"+openAIWSTurnStateHeader] = w.turnStateValue(state.Str)
				} else {
					out[source+":"+openAIWSTurnStateHeader] = responsesCacheDiagnosticValue{Bytes: len(state.Raw), State: "invalid"}
				}
			}
			if embedded := meta.Get(openAIWSTurnMetadataHeader); embedded.Exists() {
				if embedded.Type == gjson.String {
					w.metadataIdentity(out, source+":turn_metadata", embedded.Str)
				} else {
					out[source+":turn_metadata"] = responsesCacheDiagnosticValue{Bytes: len(embedded.Raw), State: "invalid"}
				}
			}
		}
	}
	if raw := fields["metadata"]; len(raw.Raw) <= gpt61SolChatIdentityMaxMetadataBytes && raw.Get("user_id").Exists() {
		source := prefix + ":claude_metadata_session"
		session, valid := validatedResponsesClaudeMetadata(raw)
		switch {
		case valid && session != "":
			out[source] = w.identityValue("session", session)
		case valid:
			// A bare account/user identifier is not a conversation. Never digest it.
			out[source] = responsesCacheDiagnosticValue{State: "no_verified_session"}
		default:
			out[source] = responsesCacheDiagnosticValue{State: "invalid"}
		}
	}
	conversation := fields["conversation"]
	if conversation.IsObject() {
		switch {
		case len(conversation.Raw) > gpt61SolChatIdentityMaxMetadataBytes:
			out[prefix+":conversation"] = responsesCacheDiagnosticValue{Bytes: len(conversation.Raw), State: "limit"}
		default:
			id := responsesCacheDiagnosticObject(conversation)["id"]
			if !id.Exists() {
				out[prefix+":conversation"] = responsesCacheDiagnosticValue{Bytes: len(conversation.Raw), State: "invalid"}
			}
			w.addIdentity(out, prefix+":conversation", "session", id)
		}
	} else {
		w.addIdentity(out, prefix+":conversation", "session", conversation)
	}
	w.addIdentity(out, prefix+":prompt_cache_key", "cache_key", fields["prompt_cache_key"])
	w.addIdentity(out, prefix+":previous_response_id", "response_id", fields["previous_response_id"])
}
