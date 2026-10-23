package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math/rand"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexWSCreateFrameOrder 复刻 codex-rs `ResponseCreateWsRequest` 的 serde
// 声明序（codex-api/src/common.rs:331-360，含 enum tag `type` 在最前）。
// Option 字段为 None 时跳过；未知键按字典序排在末尾。
var codexWSCreateFrameOrder = [...]string{
	"type",
	"model",
	"stream",
	"service_tier",
	"previous_response_id",
	"input",
	"tools",
	"tool_choice",
	"parallel_tool_calls",
	"reasoning",
	"store",
	"stream_options",
	"include",
	"prompt_cache_key",
	"text",
	"generate",
	"client_metadata",
	"access_programs",
}

// codexWSRewrittenKeys 是网关自己改写过的字段集合：这些字段必须从 payload
// map 取值；其余字段优先透传原始请求体字节，保持入站客户端的原始线序与
// 编码细节（尤其 tools/reasoning 这类可能含深层结构的值）。
var codexWSRewrittenKeys = map[string]struct{}{
	"type":                 {},
	"stream":               {},
	"previous_response_id": {},
	"input":                {},
	"store":                {},
	"generate":             {},
	"client_metadata":      {},
	"background":           {},
}

// codexWSClientMetadataOrder 是 client_metadata 的合法键集合（抓包并集）。
// 官方用 Rust HashMap（每请求随机序）；本网关在完整集合上按 turn/session 做
// 确定性洗牌，保证 HTTP body 与 WS 帧的相对顺序一致。
var codexWSClientMetadataOrder = [...]string{
	"x-codex-ws-stream-request-start-ms",
	"guardian_credits_requested",
	"x-codex-installation-id",
	"session_id",
	"turn_id",
	"ws_request_header_x_openai_internal_codex_responses_lite",
	"x-codex-window-id",
	"mcp_attribution",
	"root_turn_id",
	"parent_turn_id",
	"x-openai-subagent",
	"x-codex-parent-thread-id",
	"x-codex-turn-state",
	"thread_id",
	"x-codex-turn-metadata",
	"ws_request_header_traceparent",
	"ws_request_header_tracestate",
}

// codexWSReasoningOrder 复刻 `Reasoning` serde 序（effort, summary, context）。
var codexWSReasoningOrder = [...]string{"effort", "summary", "context"}

// codexWSTextOrder 复刻 `TextControls` serde 序（verbosity, format）。
var codexWSTextOrder = [...]string{"verbosity", "format"}

// codexWSTextFormatOrder 复刻 `TextFormat` serde 序（type, strict, schema, name）。
var codexWSTextFormatOrder = [...]string{"type", "strict", "schema", "name"}

// codexWSStreamOptionsOrder 复刻 `StreamOptions` serde 序。
var codexWSStreamOptionsOrder = [...]string{"reasoning_summary_delivery"}

// marshalCodexWSCreateFrame 把最终 WS payload 序列化成与官方
// `ResponseCreateWsRequest` serde 输出逐字节一致的帧：
//   - 顶层键按 codexWSCreateFrameOrder，未知键字典序追加；
//   - rewritten 字段取 payload map（经结构化编码保持子对象键序）；
//   - 未改字段直接引用原始请求体 raw 字节，原样保留客户端的编码；
//   - input 为 []json.RawMessage / RawMessage 混合时逐条透传；
//   - client_metadata 用固定 HashMap 样本序；
//   - 非 ASCII 走 escapeCodexNonASCII（与官方 to_ascii_json_string 一致）。
//
//nolint:errcheck // bytes.Buffer 写入不会失败
func marshalCodexWSCreateFrame(body []byte, payload map[string]any, jitter bool) ([]byte, error) {
	if payload == nil {
		return []byte("null"), nil
	}
	payload = applyCodexWSFrameDefaults(payload)
	var buf bytes.Buffer
	buf.WriteByte('{')
	emitted := make(map[string]struct{}, len(codexWSCreateFrameOrder))
	first := true
	writeField := func(key string) error {
		value, exists := payload[key]
		if !exists || value == nil {
			return nil
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		emitted[key] = struct{}{}
		writeCodexJSONString(&buf, key)
		buf.WriteByte(':')
		return writeCodexWSFrameFieldValue(&buf, body, key, value, jitter)
	}
	for _, key := range codexWSCreateFrameOrder[:] {
		if err := writeField(key); err != nil {
			return nil, err
		}
	}
	var extra []string
	for key := range payload {
		if _, done := emitted[key]; !done {
			if _, rewritten := codexWSRewrittenKeys[key]; !rewritten || key == "background" {
				extra = append(extra, key)
			}
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		if err := writeField(key); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return escapeCodexNonASCII(buf.Bytes()), nil
}

// applyCodexWSFrameDefaults 在序列化副本上补齐官方 ResponseCreateWsRequest 的恒发
// 字段（codex-rs/core/src/client.rs build_responses_request：stream 恒 true、
// tool_choice 恒 "auto"、store 恒 false、include/parallel_tool_calls/input 按
// struct 字段恒序列化、reasoning 恒 Some——不支持推理的模型官方也会发出 {}）。
// 只在缺省时注入，客户端显式给的值（除 stream 外）保持原样；stream 强制 true 与
// 官方一致（上游在该端点总是回事件流）。返回副本避免污染调用方共享的 payload。
func applyCodexWSFrameDefaults(payload map[string]any) map[string]any {
	next := make(map[string]any, len(payload)+6)
	for k, v := range payload {
		next[k] = v
	}
	if _, ok := next["input"]; !ok {
		next["input"] = []any{}
	}
	if _, ok := next["tool_choice"]; !ok {
		next["tool_choice"] = "auto"
	}
	if _, ok := next["parallel_tool_calls"]; !ok {
		next["parallel_tool_calls"] = false
	}
	if _, ok := next["reasoning"]; !ok {
		next["reasoning"] = map[string]any{}
	}
	if _, ok := next["store"]; !ok {
		next["store"] = false
	}
	if _, ok := next["include"]; !ok {
		next["include"] = []any{}
	}
	next["stream"] = true
	return next
}

//nolint:errcheck // bytes.Buffer 写入不会失败
func writeCodexWSFrameFieldValue(buf *bytes.Buffer, body []byte, key string, value any, jitter bool) error {
	switch key {
	case "input":
		return writeCodexWSInputField(buf, value)
	case "client_metadata":
		return writeCodexWSClientMetadata(buf, value, jitter)
	// reasoning/text/stream_options 在官方是强类型 struct，客户端不可能发出
	// 任意字节——始终归一到 serde 声明序而非透传。
	case "reasoning", "text", "stream_options":
		return writeCodexWSShapedValue(buf, key, value)
	}
	// 未被网关改写的字段优先透传原始请求体字节（客户端的原始键序/编码细节）。
	if _, rewritten := codexWSRewrittenKeys[key]; !rewritten && len(body) > 0 {
		if raw := gjson.GetBytes(body, key); raw.Exists() && strings.TrimSpace(raw.Raw) != "" {
			buf.WriteString(raw.Raw)
			return nil
		}
	}
	return writeCodexJSONLeaf(buf, value)
}

// writeCodexWSInputField 输出 input：RawMessage 项逐条透传原始字节。
//
//nolint:errcheck // bytes.Buffer 写入不会失败
func writeCodexWSInputField(buf *bytes.Buffer, value any) error {
	switch items := value.(type) {
	case json.RawMessage:
		if len(items) > 0 {
			buf.Write(items)
			return nil
		}
	case []json.RawMessage:
		buf.WriteByte('[')
		for i, item := range items {
			if i > 0 {
				buf.WriteByte(',')
			}
			if len(item) == 0 {
				buf.WriteString("null")
				continue
			}
			buf.Write(item)
		}
		buf.WriteByte(']')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, item := range items {
			if i > 0 {
				buf.WriteByte(',')
			}
			if raw, ok := item.(json.RawMessage); ok && len(raw) > 0 {
				buf.Write(raw)
				continue
			}
			if err := writeCodexJSONLeaf(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	}
	return writeCodexJSONLeaf(buf, value)
}

// writeCodexWSClientMetadata 输出 client_metadata：键序按请求在合法键集合内
// 打乱（官方客户端是 Rust HashMap，每次请求顺序随机），值统一为字符串。
//
//nolint:errcheck // bytes.Buffer 写入不会失败
func writeCodexWSClientMetadata(buf *bytes.Buffer, value any, jitter bool) error {
	metadata, ok := value.(map[string]any)
	if !ok {
		return writeCodexJSONLeaf(buf, value)
	}
	keys := orderedCodexJSONKeys(metadata, codexWSClientMetadataOrder[:])
	if jitter {
		keys = codexShuffledClientMetadataKeys(metadata)
	}
	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeCodexJSONString(buf, key)
		buf.WriteByte(':')
		if err := writeCodexJSONLeaf(buf, metadata[key]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// writeCodexWSShapedValue 输出带固定键序的小型对象（reasoning/text 等）。
//
//nolint:errcheck // bytes.Buffer 写入不会失败
func writeCodexWSShapedValue(buf *bytes.Buffer, key string, value any) error {
	object, ok := value.(map[string]any)
	if !ok {
		return writeCodexJSONLeaf(buf, value)
	}
	var order []string
	switch key {
	case "reasoning":
		order = codexWSReasoningOrder[:]
	case "text":
		order = codexWSTextOrder[:]
	case "stream_options":
		order = codexWSStreamOptionsOrder[:]
	}
	keys := orderedCodexJSONKeys(object, order)
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeCodexJSONString(buf, k)
		buf.WriteByte(':')
		// text.format 是嵌套的 TextFormat struct，同样按 serde 序输出。
		if key == "text" && k == "format" {
			if err := writeCodexWSOrderedObject(buf, object[k], codexWSTextFormatOrder[:]); err != nil {
				return err
			}
			continue
		}
		if err := writeCodexJSONLeaf(buf, object[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// writeCodexWSOrderedObject 用固定键序输出一个对象（嵌套 struct）。
//
//nolint:errcheck // bytes.Buffer 写入不会失败
func writeCodexWSOrderedObject(buf *bytes.Buffer, value any, order []string) error {
	object, ok := value.(map[string]any)
	if !ok {
		return writeCodexJSONLeaf(buf, value)
	}
	keys := orderedCodexJSONKeys(object, order)
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeCodexJSONString(buf, k)
		buf.WriteByte(':')
		if err := writeCodexJSONLeaf(buf, object[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// codexShuffledClientMetadataKeys returns a per-request permutation of the
// client_metadata keys. The official client serializes a Rust HashMap, so any
// permutation is legal; deriving it from the turn/session id keeps retries of
// the same turn stable while different turns get different orders.
func codexShuffledClientMetadataKeys(metadata map[string]any) []string {
	present := make(map[string]struct{}, len(metadata))
	values := make(map[string]string, len(metadata))
	for key, value := range metadata {
		present[key] = struct{}{}
		values[key] = codexClientMetadataOrderValue(value)
	}
	return codexShuffledClientMetadataKeySet(present, codexClientMetadataOrderSeed(values))
}

// codexShuffledClientMetadataKeySet shuffles the full legal key order, then
// filters to the keys present. Shuffling the full list keeps the relative order
// of shared keys identical between the HTTP body and WS frame even when one
// lane carries an extra key (for example x-codex-turn-state).
func codexShuffledClientMetadataKeySet(present map[string]struct{}, seed string) []string {
	candidates := make([]string, 0, len(codexWSClientMetadataOrder)+len(present))
	seen := make(map[string]bool, len(codexWSClientMetadataOrder))
	for _, key := range codexWSClientMetadataOrder {
		if !seen[key] {
			candidates = append(candidates, key)
			seen[key] = true
		}
	}
	candidates = codexShuffleKeys(candidates, seed)
	keys := make([]string, 0, len(present))
	for _, key := range candidates {
		if _, ok := present[key]; ok {
			keys = append(keys, key)
		}
	}
	extra := make([]string, 0, len(present))
	for key := range present {
		if !seen[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	return append(keys, extra...)
}

func codexClientMetadataOrderValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

func codexClientMetadataOrderSeed(values map[string]string) string {
	for _, key := range []string{"turn_id", "session_id"} {
		if value := strings.TrimSpace(values[key]); value != "" {
			return key + ":" + value
		}
	}
	if raw := strings.TrimSpace(values[openAIWSTurnMetadataHeader]); raw != "" {
		if turn := strings.TrimSpace(gjson.Get(raw, "turn_id").String()); turn != "" {
			return "turn_id:" + turn
		}
		if session := strings.TrimSpace(gjson.Get(raw, "session_id").String()); session != "" {
			return "session_id:" + session
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		_, _ = b.WriteString(key)
		_ = b.WriteByte('=')
		_, _ = b.WriteString(values[key])
		_ = b.WriteByte(0)
	}
	return b.String()
}

func codexShuffleKeys(keys []string, seed string) []string {
	sort.Strings(keys)
	h := sha256.Sum256([]byte(seed))
	r := rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(h[:8]))))
	r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	return keys
}

// reorderCodexClientMetadataJSON applies the same per-request HashMap order to
// the HTTP body lane so both transports expose one consistent ordering.
func reorderCodexClientMetadataJSON(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	cm := gjson.GetBytes(body, "client_metadata")
	if !cm.IsObject() {
		return body, false
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cm.Raw), &members); err != nil || len(members) == 0 {
		return body, false
	}
	present := make(map[string]struct{}, len(members))
	values := make(map[string]string, len(members))
	for key, raw := range members {
		present[key] = struct{}{}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			values[key] = text
		} else {
			values[key] = string(raw)
		}
	}
	keys := codexShuffledClientMetadataKeySet(present, codexClientMetadataOrderSeed(values))
	var buf bytes.Buffer
	_ = buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			_ = buf.WriteByte(',')
		}
		writeCodexJSONString(&buf, key)
		_ = buf.WriteByte(':')
		_, _ = buf.Write(members[key])
	}
	_ = buf.WriteByte('}')
	next, err := sjson.SetRawBytes(body, "client_metadata", buf.Bytes())
	if err != nil {
		return body, false
	}
	return next, !bytes.Equal(next, body)
}

// codexHTTPRequestTopLevelOrder mirrors codex-rs ResponsesApiRequest serde
// declaration order (codex-api/src/common.rs:279-304). HTTP bodies must put
// routing fields first for the same reason the official client does.
var codexHTTPRequestTopLevelOrder = [...]string{
	"model",
	"stream",
	"service_tier",
	"input",
	"tools",
	"tool_choice",
	"parallel_tool_calls",
	"reasoning",
	"store",
	"stream_options",
	"include",
	"prompt_cache_key",
	"text",
	"client_metadata",
	"access_programs",
}

// reorderCodexRequestTopLevelJSON rewrites the top-level key order to the
// official struct order. Values are copied verbatim, so nested item ordering
// and integer formatting are untouched.
func reorderCodexRequestTopLevelJSON(body []byte) ([]byte, bool) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body, false
	}
	type member struct {
		key string
		raw string
	}
	members := make(map[string]member, 16)
	original := make([]string, 0, 16)
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if _, exists := members[name]; exists {
			return false
		}
		members[name] = member{key: key.Raw, raw: value.Raw}
		original = append(original, name)
		return true
	})
	if len(members) == 0 {
		return body, false
	}
	order := make([]string, 0, len(members))
	seen := make(map[string]bool, len(members))
	for _, name := range codexHTTPRequestTopLevelOrder {
		if _, ok := members[name]; ok {
			order = append(order, name)
			seen[name] = true
		}
	}
	rest := make([]string, 0, len(members))
	for name := range members {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	order = append(order, rest...)
	changed := len(order) != len(original)
	if !changed {
		for i := range order {
			if order[i] != original[i] {
				changed = true
				break
			}
		}
	}
	if !changed {
		return body, false
	}
	var buf bytes.Buffer
	_ = buf.WriteByte('{')
	for i, name := range order {
		if i > 0 {
			_ = buf.WriteByte(',')
		}
		_, _ = buf.WriteString(members[name].key)
		_ = buf.WriteByte(':')
		_, _ = buf.WriteString(members[name].raw)
	}
	_ = buf.WriteByte('}')
	return buf.Bytes(), true
}
