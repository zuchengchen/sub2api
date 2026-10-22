package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Turn metadata is also used as an HTTP header, including when carried in WS JSON.

// codexTurnMetadataKeyOrder 复刻官方 CodexTurnMetadataPayload 的 serde 字段顺序
// （codex-rs/core/src/responses_metadata.rs:544-601，全部 skip_serializing_if=none）。
// serde 按 struct 声明顺序序列化，未知键走 #[serde(flatten)] 的 BTreeMap<String,String>
// （字典序、排在末尾）。Go map 的 json.Marshal 恒为字母序，与官方线序不一致，
// 上游可逐字节比对——必须显式复刻。
var codexTurnMetadataKeyOrder = [...]string{
	"installation_id",
	"session_id",
	"thread_id",
	"agent_name",
	"turn_id",
	"window_id",
	"window_number",
	"context_window_id",
	"request_kind",
	"history_initialization",
	"forked_from_thread_id",
	"forked_from_ordinal_exclusive",
	"parent_thread_id",
	"parent_turn_id",
	"root_turn_id",
	"subagent_kind",
	"thread_source",
	"turn_trigger",
	"sandbox",
	"sandbox_mode",
	"auto_review_enabled",
	"node_repl_auto_review_required",
	"node_repl_disabled",
	"workspaces",
	"tool_namespaces_info",
	"turn_started_at_unix_ms",
	"history_ingest_requested",
	"analytics_enabled",
	"compaction",
}

// codexJSONShape 描述一类官方 JSON 对象的线序形状。serde 语义下：
//   - order 非空：该层是 struct，键按 order（声明序）输出，order 之外的未知键
//     按字典序追加（复刻 flatten 的 BTreeMap）。
//   - order 为空：该层是 BTreeMap，键按字典序输出。
//   - fields：struct 内某键指向的子对象 kind（缺省为叶子）。
//   - values：BTreeMap 的每个 value 指向的子对象 kind（缺省为叶子）。
type codexJSONShape struct {
	order  []string
	fields map[string]string
	values string
}

// codexJSONShapes 复刻官方 turn metadata 里出现的各类对象形状。
// 依据 codex-rs core/src/responses_metadata.rs（payload 544-601、workspace 186-194、
// tool namespace 196-222）与 analytics/src/facts.rs（compaction 枚举）。
var codexJSONShapes = map[string]codexJSONShape{
	"": { // CodexTurnMetadataPayload 顶层
		order: codexTurnMetadataKeyOrder[:],
		fields: map[string]string{
			"workspaces":           "workspaces",
			"tool_namespaces_info": "tool_namespaces_info",
			"compaction":           "compaction",
		},
	},
	"workspaces":           {values: "workspace"}, // BTreeMap<path, TurnMetadataWorkspace>
	"workspace":            {order: []string{"associated_remote_urls", "latest_git_commit_hash", "has_changes"}, fields: map[string]string{"associated_remote_urls": "remote_urls"}},
	"remote_urls":          {},                    // BTreeMap<name, SanitizedGitUrl(String)>
	"tool_namespaces_info": {values: "namespace"}, // BTreeMap<name, TurnToolNamespaceInfo>
	"namespace":            {order: []string{"name", "functions"}, fields: map[string]string{"functions": "functions"}},
	"functions":            {values: "function"}, // BTreeMap<name, TurnToolFunctionInfo>
	"function":             {order: []string{"name", "direct", "code_mode_name", "deferred", "source"}},
	"compaction":           {order: []string{"trigger", "reason", "implementation", "phase", "strategy"}},
}

// orderedCodexJSONKeys 先按官方顺序取已出现的键，再把未知键按字典序追加，
// 复刻 serde struct + flatten(BTreeMap) 的混合输出。
func orderedCodexJSONKeys(object map[string]any, order []string) []string {
	keys := make([]string, 0, len(object))
	seen := make(map[string]struct{}, len(order))
	for _, key := range order {
		if _, exists := object[key]; exists {
			keys = append(keys, key)
			seen[key] = struct{}{}
		}
	}
	rest := make([]string, 0, len(object)-len(seen))
	for key := range object {
		if _, ok := seen[key]; !ok {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// writeCodexJSONString 只转义 JSON 语法必需的字节；非 ASCII 留给
// escapeCodexNonASCII 统一处理（官方 AsciiJsonFormatter 也是两层语义）。
// 与官方一致：& < > 不转义。
//
//nolint:errcheck // bytes.Buffer 写入不会失败
func writeCodexJSONString(buf *bytes.Buffer, value string) {
	buf.WriteByte('"')
	start := 0
	for i := range len(value) {
		switch b := value[i]; b {
		case '"', '\\':
			buf.WriteString(value[start:i])
			buf.WriteByte('\\')
			buf.WriteByte(b)
			start = i + 1
		case '\n':
			buf.WriteString(value[start:i])
			buf.WriteString(`\n`)
			start = i + 1
		case '\r':
			buf.WriteString(value[start:i])
			buf.WriteString(`\r`)
			start = i + 1
		case '\t':
			buf.WriteString(value[start:i])
			buf.WriteString(`\t`)
			start = i + 1
		default:
			if b < 0x20 {
				buf.WriteString(value[start:i])
				fmt.Fprintf(buf, `\u%04x`, b)
				start = i + 1
			}
		}
	}
	buf.WriteString(value[start:])
	buf.WriteByte('"')
}

// writeCodexJSONLeaf 用关闭 HTML 转义的编码器输出标量与数组。
//
//nolint:errcheck // bytes.Buffer 写入不会失败
func writeCodexJSONLeaf(buf *bytes.Buffer, value any) error {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	out := encoded.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	buf.Write(out)
	return nil
}

//nolint:errcheck // bytes.Buffer 写入不会失败
func writeCodexJSONObject(buf *bytes.Buffer, object map[string]any, kind string) error {
	shape := codexJSONShapes[kind]
	keys := orderedCodexJSONKeys(object, shape.order)
	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeCodexJSONString(buf, key)
		buf.WriteByte(':')
		child, ok := object[key].(map[string]any)
		if !ok {
			if err := writeCodexJSONLeaf(buf, object[key]); err != nil {
				return err
			}
			continue
		}
		childKind := shape.fields[key]
		if childKind == "" {
			childKind = shape.values
		}
		if err := writeCodexJSONObject(buf, child, childKind); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// escapeCodexNonASCII 复刻官方 to_ascii_json_string 的非 ASCII 转义
// （codex-rs/utils/string/src/json.rs:12-38）：只把非 ASCII 码点写成小写 \uXXXX，
// ASCII（含 & < > 与 U+007F）原样保留。官方仅在字符串片段上做该转义，
// 而合法 JSON 里的高位字节只可能出现在字符串内，故字节级后处理等价。
func escapeCodexNonASCII(raw []byte) []byte {
	for i, b := range raw {
		if b < 0x7f {
			continue
		}
		out := make([]byte, 0, len(raw)+16)
		out = append(out, raw[:i]...)
		appendEscape := func(r rune) {
			const hex = "0123456789abcdef"
			out = append(out, '\\', 'u', hex[r>>12&15], hex[r>>8&15], hex[r>>4&15], hex[r&15])
		}
		for _, r := range string(raw[i:]) {
			switch {
			case r < 0x7f:
				out = append(out, byte(r))
			case r <= 0xffff:
				appendEscape(r)
			default:
				high, low := utf16.EncodeRune(r)
				appendEscape(high)
				appendEscape(low)
			}
		}
		return out
	}
	return raw
}

func marshalCodexTurnMetadata(metadata map[string]any) ([]byte, error) {
	if metadata == nil {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	if err := writeCodexJSONObject(&buf, metadata, ""); err != nil {
		return nil, err
	}
	return escapeCodexNonASCII(buf.Bytes()), nil
}

// Keep business metadata integers lossless when rewriting only identity fields.
func decodeCodexMetadataObject(raw string, target *map[string]any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("multiple JSON values in Codex metadata")
	}
	return nil
}

func decodeCodexTurnMetadata(raw string) (map[string]any, error) {
	var metadata map[string]any
	err := decodeCodexMetadataObject(raw, &metadata)
	return metadata, err
}

// normalizeCodexRequestMetadata runs after account/fingerprint projection. It
// never scopes an already-scoped ID. Core's embedded metadata is canonical;
// flat client_metadata and transport headers are compatibility projections.
// isCompact additionally identifies the legacy /responses/compact endpoint.
func normalizeCodexRequestMetadata(body []byte, headers http.Header, account *Account, isCompact bool) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAIOAuthLike() || !gjson.ParseBytes(body).IsObject() {
		return body, false, nil
	}
	if isCompact {
		// Legacy CompactionInput has no client_metadata field. compact 走独立上游契约、
		// 不过 projectCodexOutboundHeaders：这里的 session 隔离值只写下划线 session_id，
		// 按同一迁移规则挪到官方连字符 session-id 再归一化。
		if headers != nil && strings.TrimSpace(headers.Get("session-id")) == "" {
			if isolated := strings.TrimSpace(headers.Get("session_id")); isolated != "" {
				headers.Set("session-id", isolated)
			}
		}
		_, err := normalizeCodexClientMetadata(map[string]any{}, headers, true)
		return body, false, err
	}
	cm := map[string]any{}
	if raw := gjson.GetBytes(body, "client_metadata"); raw.IsObject() {
		if err := decodeCodexMetadataObject(raw.Raw, &cm); err != nil {
			return body, false, err
		}
	} else if raw.Exists() {
		return body, false, nil
	}
	defaultsChanged, err := ensureCodexClientMetadataDefaults(cm, headers, body, account, false)
	if err != nil {
		return body, false, err
	}
	changed, err := normalizeCodexClientMetadata(cm, headers, HasCompactionTriggerInInput(body))
	if err != nil {
		return body, false, err
	}
	changed = changed || defaultsChanged
	if !changed {
		return body, false, nil
	}
	raw, err := json.Marshal(cm)
	if err != nil {
		return body, false, err
	}
	next := body
	next, err = sjson.SetRawBytes(next, "client_metadata", raw)
	if err != nil {
		return body, false, err
	}
	return next, true, nil
}

func normalizeCodexClientMetadata(cm map[string]any, headers http.Header, isCompact bool) (bool, error) {
	metadata := map[string]any{}
	if raw, ok := cm[openAIWSTurnMetadataHeader].(string); ok {
		if decoded, err := decodeCodexTurnMetadata(raw); err == nil && decoded != nil {
			metadata = decoded
		}
	}
	if raw := headers.Get(openAIWSTurnMetadataHeader); raw != "" {
		if decoded, err := decodeCodexTurnMetadata(raw); err == nil {
			for key, value := range decoded {
				if _, exists := metadata[key]; !exists {
					metadata[key] = value
				}
			}
		}
	}
	changed := false
	set := func(values map[string]any, key string, value any) {
		if current, exists := values[key]; exists && current == value {
			return
		}
		values[key] = value
		changed = true
	}
	for _, field := range []struct {
		metadata string
		flat     string
		header   string
	}{
		{"installation_id", "x-codex-installation-id", "x-codex-installation-id"},
		{"session_id", "session_id", "session-id"},
		{"thread_id", "thread_id", "thread-id"},
		{"turn_id", "turn_id", "turn-id"},
		{"window_id", "x-codex-window-id", "x-codex-window-id"},
	} {
		value, _ := metadata[field.metadata].(string)
		if value == "" {
			value, _ = cm[field.flat].(string)
		}
		if value == "" {
			value, _ = cm[field.metadata].(string)
		}
		if value == "" {
			value = headers.Get(field.header)
		}
		if value == "" {
			continue
		}
		set(cm, field.flat, value)
		if _, exists := cm[field.metadata]; exists {
			set(cm, field.metadata, value)
		}
		if len(metadata) != 0 {
			set(metadata, field.metadata, value)
		}
		if headers != nil && (field.metadata == "session_id" || field.metadata == "thread_id" || headers.Get(field.header) != "") {
			headers.Set(field.header, value)
		}
	}
	if headers != nil {
		if session, _ := cm["session_id"].(string); session != "" && headers.Get("session_id") != "" {
			headers.Set("session_id", session)
		}
		if thread, _ := cm["thread_id"].(string); thread != "" && headers.Get("x-client-request-id") != "" {
			headers.Set("x-client-request-id", thread)
		}
	}
	if isCompact {
		set(metadata, "request_kind", "compaction")
		if _, exists := cm["request_kind"]; exists {
			set(cm, "request_kind", "compaction")
		}
	}
	if len(metadata) != 0 {
		raw, err := marshalCodexTurnMetadata(metadata)
		if err != nil {
			return false, err
		}
		set(cm, openAIWSTurnMetadataHeader, string(raw))
		if headers != nil {
			headerRaw, err := sjson.DeleteBytes(raw, "tool_namespaces_info")
			if err != nil {
				return false, err
			}
			headers.Set(openAIWSTurnMetadataHeader, string(headerRaw))
		}
	}
	return changed, nil
}

func ensureCodexClientMetadataDefaults(cm map[string]any, headers http.Header, body []byte, account *Account, isCompact bool) (bool, error) {
	if cm == nil || account == nil {
		return false, nil
	}
	metadata := map[string]any{}
	if raw, ok := cm[openAIWSTurnMetadataHeader].(string); ok && strings.TrimSpace(raw) != "" {
		if decoded, err := decodeCodexTurnMetadata(raw); err == nil && decoded != nil {
			metadata = decoded
		}
	}
	if headers != nil {
		if raw := headers.Get(openAIWSTurnMetadataHeader); strings.TrimSpace(raw) != "" {
			if decoded, err := decodeCodexTurnMetadata(raw); err == nil {
				for key, value := range decoded {
					if _, exists := metadata[key]; !exists {
						metadata[key] = value
					}
				}
			}
		}
	}
	changed := false
	set := func(values map[string]any, key, value string) string {
		if strings.TrimSpace(value) == "" {
			return ""
		}
		if current, ok := values[key].(string); ok && strings.TrimSpace(current) != "" {
			return strings.TrimSpace(current)
		}
		values[key] = value
		changed = true
		return value
	}
	from := func(values ...any) string {
		return firstNonEmptyString(values...)
	}
	installationID := from(metadata["installation_id"], cm["x-codex-installation-id"], cm["installation_id"])
	if installationID == "" && headers != nil {
		installationID = strings.TrimSpace(headers.Get("x-codex-installation-id"))
	}
	if installationID == "" {
		installationID = strings.TrimSpace(account.GetOpenAIDeviceID())
	}
	if installationID == "" {
		installationID = deriveStableUUIDv4(fmt.Sprintf("sub2api:codex-installation:v1:%d", account.ID))
	}
	sessionID := from(metadata["session_id"], cm["session_id"])
	if sessionID == "" && headers != nil {
		sessionID = from(headers.Get("session-id"), headers.Get("session_id"), headers.Get("conversation_id"))
	}
	identitySeed := codexIdentityClockSeed(account)
	if sessionID == "" {
		// Raw body key: isolate per credential before it becomes an identity.
		if key := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); key != "" {
			sessionID = isolateOpenAIUpstreamSessionID(0, account, key)
		}
	}
	if sessionID == "" {
		sessionID = deriveCodexFallbackSessionID(account, body)
	}
	// Official session ids are UUIDv7. Isolated cache keys (16 hex) or custom
	// keys must not reach the wire as session-id; map them to a stable v7.
	sessionID = canonicalCodexSessionHeaderValue(sessionID)
	threadID := from(metadata["thread_id"], cm["thread_id"])
	if threadID == "" && headers != nil {
		threadID = from(headers.Get("thread-id"), headers.Get("thread_id"), headers.Get("x-client-request-id"))
	}
	if threadID == "" {
		threadID = sessionID
	}
	threadID = canonicalCodexSessionHeaderValue(threadID)
	turnID := from(metadata["turn_id"], cm["turn_id"])
	if turnID == "" && headers != nil {
		turnID = strings.TrimSpace(headers.Get("turn-id"))
	}
	var turnStartedAt int64
	if turnID == "" {
		// 官方 turn_id 是每回合一个 Uuid::now_v7()，同一回合的工具循环请求复用
		// 同一 turn_id 与 turn_started_at（golden 0.163.0）。按最后一条 user
		// 消息派生回合，而不是按整个请求体——后者每个工具循环请求都不同。
		turnID, turnStartedAt = resolveCodexTurnIdentity(defaultCodexTurnClock, identitySeed, threadID,
			codexTurnEvidence{InputTurnSeed: codexInputTurnSeed(gjson.GetBytes(body, "input"))})
	}
	windowID := from(metadata["window_id"], cm["x-codex-window-id"], cm["window_id"])
	if windowID == "" && headers != nil {
		windowID = strings.TrimSpace(headers.Get("x-codex-window-id"))
	}
	if windowID == "" {
		windowID = threadID + ":0"
	}
	// window_number / context_window_id 是官方 turn metadata 的两个独立字段：
	// window_number 是当前自动压缩窗口序号（u64，新会话从 0 起，
	// core/src/state/auto_compact_window.rs:49-66），context_window_id 是独立的
	// Uuid::now_v7()（core/src/session/mod.rs:4484）。代理只转发不压缩，窗口序号
	// 恒为 0；context_window_id 从 thread 派生 v7，保持"每窗口一个 UUID"的形状。
	windowNumber := int64(0)
	if raw, ok := metadata["window_number"]; ok {
		switch value := raw.(type) {
		case json.Number:
			if parsed, err := value.Int64(); err == nil && parsed >= 0 {
				windowNumber = parsed
			}
		case float64:
			if value >= 0 {
				windowNumber = int64(value)
			}
		}
	}
	contextWindowID := from(metadata["context_window_id"])
	if contextWindowID == "" {
		contextWindowID = deriveStableCodexUUIDv7("sub2api:codex-context-window:v1:"+threadID+":"+strconv.FormatInt(windowNumber, 10), deriveCodexStableTimestampMs(threadID))
	}
	requestKind := from(metadata["request_kind"])
	if isCompact {
		requestKind = "compaction"
	} else if requestKind == "" {
		requestKind = "turn"
	}

	set(cm, "x-codex-installation-id", installationID)
	set(cm, "session_id", sessionID)
	set(cm, "thread_id", threadID)
	set(cm, "turn_id", turnID)
	set(cm, "x-codex-window-id", windowID)
	set(metadata, "installation_id", installationID)
	set(metadata, "session_id", sessionID)
	set(metadata, "thread_id", threadID)
	set(metadata, "turn_id", turnID)
	set(metadata, "window_id", windowID)
	set(metadata, "request_kind", requestKind)
	if _, exists := metadata["window_number"]; !exists {
		metadata["window_number"] = windowNumber
		changed = true
	}
	if _, exists := metadata["context_window_id"]; !exists {
		metadata["context_window_id"] = contextWindowID
		changed = true
	}
	if _, exists := metadata["turn_started_at_unix_ms"]; !exists {
		if turnStartedAt <= 0 {
			turnStartedAt = codexUUIDv7Millis(turnID)
		}
		if turnStartedAt <= 0 {
			turnStartedAt = time.Now().UnixMilli()
		}
		metadata["turn_started_at_unix_ms"] = turnStartedAt
		changed = true
	}
	raw, err := marshalCodexTurnMetadata(metadata)
	if err != nil {
		return false, err
	}
	if current, ok := cm[openAIWSTurnMetadataHeader].(string); !ok || current != string(raw) {
		cm[openAIWSTurnMetadataHeader] = string(raw)
		changed = true
	}
	if headers != nil {
		headers.Set("x-codex-installation-id", installationID)
		headers.Set("session-id", sessionID)
		headers.Set("thread-id", threadID)
		headers.Set("x-client-request-id", threadID)
		headers.Set("x-codex-window-id", windowID)
		// Official HTTP traffic has no underscored session_id (golden 0.163.0);
		// projectCodexOutboundHeaders strips it on the normal paths, and the
		// compat bridges no longer depend on it.
		headers.Del("session_id")
		headerRaw, err := sjson.DeleteBytes(raw, "tool_namespaces_info")
		if err != nil {
			return false, err
		}
		headers.Set(openAIWSTurnMetadataHeader, string(headerRaw))
	}
	return changed, nil
}

// codexIdentityClockSeed namespaces derived session/turn ids per account:
// the account's fingerprint seed when present, else its id.
func codexIdentityClockSeed(account *Account) string {
	if account == nil {
		return ""
	}
	if seed, ok := codexFingerprintSeed(account.Extra); ok {
		return seed
	}
	return "account:" + strconv.FormatInt(account.ID, 10)
}

// canonicalCodexSessionHeaderValue maps an isolated session key to the value
// sent as session-id. Official session ids are UUIDv7; the 16-hex isolation
// keys must not reach the wire. HTTP (ensureCodexClientMetadataDefaults) and
// WS handshakes apply the same mapping so both lanes agree.
//
// Callers pass an already-isolated key (isolateOpenAIUpstreamSessionID mixes in
// the API key and the credential account), so the mapping itself uses a fixed
// seed: shadow/agent-identity/OAuth views of one credential and the HTTP/WS
// lanes then agree byte for byte.
func canonicalCodexSessionHeaderValue(sessionKey string) string {
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" || isCanonicalUUID(sessionKey) {
		return sessionKey
	}
	return resolveCodexSessionIdentity(defaultCodexTurnClock, "sub2api:codex-session-canon:v1", sessionKey)
}

func isCanonicalUUID(raw string) bool {
	parsed, err := uuid.Parse(strings.TrimSpace(raw))
	return err == nil && parsed.String() == strings.TrimSpace(raw)
}

// deriveCodexFallbackSessionID 为完全未携带会话信号（无 header、无
// prompt_cache_key、无 metadata）的客户端合成 session_id。旧实现只按
// account.ID 派生，同一账号下所有互不相干的对话塌缩成同一个 session_id，
// 上游一眼看出「一个会话跑遍所有话题」。
// 关联键 = 账号 + 会话判别信号，优先级与已删除的内容审计分组规则一致
// （先 join ResponseID/PreviousResponseID 的 'r' 链，再 join 's' 会话哈希）：
// 先真实响应链的 previous_response_id，再退到带用户锚点的 system+首条
// user 内容哈希，两者皆无才退回纯账号键。不同账号即便内容相同也不共享
// session_id——满足「会话+账号」关联。
func deriveCodexFallbackSessionID(account *Account, body []byte) string {
	// 先真实响应链：只认上游签发的 resp_* 形态（ClassifyOpenAIPreviousResponseIDKind），
	// 客户端自造的 id 不得成为关联键。
	var discriminator string
	if respID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()); respID != "" &&
		ClassifyOpenAIPreviousResponseIDKind(respID) == OpenAIPreviousResponseIDKindResponseID {
		discriminator = "resp:" + respID
	} else if anchor := deriveOpenAIAnchoredContentSessionSeed(body); anchor != "" {
		// anchored 变体必需：无用户锚点时它返回空。deriveOpenAIContentSessionSeed
		// 会把 model+tools 当成会话身份，同模型同工具的所有对话又会塌缩回同一个
		// session_id——正是本函数要修的重复问题。
		discriminator = anchor
	}
	seed := fmt.Sprintf("sub2api:codex-session:v2:%d:%s", account.ID, discriminator)
	return deriveStableCodexUUIDv7(seed, deriveCodexStableTimestampMs(seed))
}

func normalizeCodexRequestMetadataMap(payload map[string]any, headers http.Header, account *Account) error {
	if account == nil || !account.IsOpenAIOAuthLike() || payload == nil {
		return nil
	}
	cm, ok := payload["client_metadata"].(map[string]any)
	if !ok {
		if existing, exists := payload["client_metadata"]; exists && existing != nil {
			return nil
		}
		cm = map[string]any{}
	}
	isCompact := false
	if input, ok := payload["input"].([]any); ok {
		for _, raw := range input {
			if item, ok := raw.(map[string]any); ok && item["type"] == "compaction_trigger" {
				isCompact = true
				break
			}
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	defaultsChanged, err := ensureCodexClientMetadataDefaults(cm, headers, body, account, isCompact)
	if err != nil {
		return err
	}
	changed, err := normalizeCodexClientMetadata(cm, headers, isCompact)
	if err != nil {
		return err
	}
	if defaultsChanged || changed {
		payload["client_metadata"] = cm
	}
	return nil
}

// applyCodexRequestMetadata keeps the HTTP request and the caller's body snapshot
// identical before transport compression.
func applyCodexRequestMetadata(req *http.Request, body []byte, account *Account, isCompact bool) ([]byte, error) {
	if req == nil {
		return body, nil
	}
	next, changed, err := normalizeCodexRequestMetadata(body, req.Header, account, isCompact)
	if err != nil {
		return body, err
	}
	if !changed {
		return body, nil
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewReader(next))
	req.ContentLength = int64(len(next))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(next)), nil
	}
	return next, nil
}

// rewriteCodexTurnMetadataJSON replaces only selected top-level identity values.
// The header and the opaque client_metadata string must retain the caller's key
// order, whitespace, Unicode escapes and unknown values. Never marshal the object.
func rewriteCodexTurnMetadataJSON(raw string, rebuildInvalid bool, updates func(map[string]any) map[string]any) string {
	original := raw
	var metadata map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if !json.Valid([]byte(raw)) || decoder.Decode(&metadata) != nil || metadata == nil {
		if !rebuildInvalid {
			return original
		}
		raw, metadata = "{}", map[string]any{}
	}
	fields := updates(metadata)
	encoded := make(map[string]string, len(fields))
	for name, value := range fields {
		next, err := marshalCodexTurnMetadataValue(value)
		if err != nil {
			return original
		}
		encoded[name] = next
	}
	out := make([]byte, 0, len(raw))
	offset := 0
	seen := make(map[string]bool, len(fields))
	gjson.Parse(raw).ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		next, ok := encoded[name]
		if !ok {
			return true
		}
		seen[name] = true
		if text, ok := fields[name].(string); ok && value.Type == gjson.String && value.Str == text {
			return true
		}
		out = append(out, raw[offset:value.Index]...)
		out = append(out, next...)
		offset = value.Index + len(value.Raw)
		return true
	})
	out = append(out, raw[offset:]...)
	next := string(out)
	for _, name := range slices.Sorted(maps.Keys(encoded)) {
		if seen[name] {
			continue
		}
		var err error
		next, err = sjson.SetRaw(next, name, encoded[name])
		if err != nil {
			return original
		}
	}
	return codexTurnMetadataASCII(next)
}

func marshalCodexTurnMetadataValue(value any) (string, error) {
	raw, err := marshalOpenAIUpstreamJSON(value)
	if err != nil {
		return "", err
	}
	return codexTurnMetadataASCII(string(raw)), nil
}

func codexTurnMetadataASCII(raw string) string {
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x80 {
			continue
		}
		out := []byte(raw[:i])
		for _, r := range raw[i:] {
			switch {
			case r < 0x80:
				out = append(out, byte(r))
			case r <= 0xffff:
				out = fmt.Appendf(out, `\u%04x`, r)
			default:
				high, low := utf16.EncodeRune(r)
				out = fmt.Appendf(out, `\u%04x\u%04x`, high, low)
			}
		}
		return string(out)
	}
	return raw
}
