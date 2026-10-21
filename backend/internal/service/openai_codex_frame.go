package service

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
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

// codexWSClientMetadataOrder 是真实客户端 client_metadata 的一组规范键序。
// 官方用 Rust HashMap（每请求随机序），这里固定为首帧抓包序——任何一个固定
// 顺序都是官方分布内的合法样本。
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
func marshalCodexWSCreateFrame(body []byte, payload map[string]any) ([]byte, error) {
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
		return writeCodexWSFrameFieldValue(&buf, body, key, value)
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
func writeCodexWSFrameFieldValue(buf *bytes.Buffer, body []byte, key string, value any) error {
	switch key {
	case "input":
		return writeCodexWSInputField(buf, value)
	case "client_metadata":
		return writeCodexWSClientMetadata(buf, value)
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

// writeCodexWSClientMetadata 输出 client_metadata：固定 HashMap 样本序，
// 值统一为字符串（官方 HashMap<String,String>）。
//
//nolint:errcheck // bytes.Buffer 写入不会失败
func writeCodexWSClientMetadata(buf *bytes.Buffer, value any) error {
	metadata, ok := value.(map[string]any)
	if !ok {
		return writeCodexJSONLeaf(buf, value)
	}
	keys := orderedCodexJSONKeys(metadata, codexWSClientMetadataOrder[:])
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
