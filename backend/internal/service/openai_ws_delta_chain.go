package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

// openAIWSDeltaChainState 是一次会话在上游侧可用的增量链快照：
// baseline = 上次逻辑 input 项哈希序列 + 上次响应全部 output item 哈希
// （对应 codex-rs get_incremental_items 的 previous_items 组合）。
// 下一次请求若 input 以 baseline 为前缀且非 input 字段指纹一致，
// 即可只发送尾部增量 + previous_response_id。
type openAIWSDeltaChainState struct {
	ResponseID      string   `json:"response_id"`
	AccountID       int64    `json:"account_id"`
	FieldsFP        string   `json:"fields_fp"`
	BaselineHashes  []string `json:"baseline"`
	UpdatedUnixNano int64    `json:"updated_unix_nano"`
}

const openAIWSDeltaChainMaxBaseline = 2048

// openAIWSDeltaItemHash 对单个 input/output item 规范化后取指纹，
// 客户端对历史项的再序列化差异（键序、空白）被消掉。
func openAIWSDeltaItemHash(item json.RawMessage) string {
	normalized := normalizeOpenAIWSJSONForCompareOrRaw(item)
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:])
}

func openAIWSDeltaItemsHash(items []json.RawMessage) []string {
	if len(items) == 0 {
		return nil
	}
	hashes := make([]string, 0, len(items))
	for _, item := range items {
		hashes = append(hashes, openAIWSDeltaItemHash(item))
	}
	return hashes
}

// openAIWSDeltaFieldsFingerprint 计算官方增量比较里的「非 input 字段」指纹。
// 官方 responses_request_properties_mismatch 逐字段比较：
// model, tools, tool_choice, parallel_tool_calls, reasoning, store, stream,
// include, service_tier, prompt_cache_key, text；
// 忽略 input, previous_response_id, stream_options, client_metadata,
// access_programs（以及 enum tag type 与网关字段）。
func openAIWSDeltaFieldsFingerprint(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	props := map[string]any{}
	for _, key := range [...]string{
		"model",
		"tools",
		"tool_choice",
		"parallel_tool_calls",
		"reasoning",
		"store",
		"stream",
		"include",
		"service_tier",
		"prompt_cache_key",
		"text",
	} {
		if value, ok := payload[key]; ok {
			props[key] = value
		}
	}
	encoded, err := marshalOpenAIUpstreamJSON(props)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// openAIWSDeltaDecision 是一次请求与既有链的匹配结果。
type openAIWSDeltaDecision struct {
	// Delta 为真时：发送 input=tailItems + previous_response_id。
	Delta            bool
	TailItems        []json.RawMessage
	PreviousResponse string
	Reason           string
}

// resolveOpenAIWSDelta 用存储链判断当前请求能否走增量。
// currentItems 是本回合将发送的逻辑 input（未做增量改写前）。
func resolveOpenAIWSDelta(chain *openAIWSDeltaChainState, accountID int64, fieldsFP string, currentItems []json.RawMessage) openAIWSDeltaDecision {
	if chain == nil {
		return openAIWSDeltaDecision{Reason: "no_chain"}
	}
	if chain.AccountID != 0 && accountID != 0 && chain.AccountID != accountID {
		return openAIWSDeltaDecision{Reason: "account_mismatch"}
	}
	if strings.TrimSpace(chain.ResponseID) == "" {
		return openAIWSDeltaDecision{Reason: "no_previous_response_id"}
	}
	if chain.FieldsFP != "" && fieldsFP != "" && chain.FieldsFP != fieldsFP {
		return openAIWSDeltaDecision{Reason: "request_property_changed"}
	}
	if len(chain.BaselineHashes) > len(currentItems) {
		return openAIWSDeltaDecision{Reason: "input_shortened"}
	}
	for idx, hash := range chain.BaselineHashes {
		if openAIWSDeltaItemHash(currentItems[idx]) != hash {
			return openAIWSDeltaDecision{Reason: "input_mismatch"}
		}
	}
	return openAIWSDeltaDecision{
		Delta:            true,
		TailItems:        append([]json.RawMessage(nil), currentItems[len(chain.BaselineHashes):]...),
		PreviousResponse: chain.ResponseID,
		Reason:           "incremental",
	}
}

// openAIWSDeltaRecorder 在一次响应流上收集链所需的增量信息：
// 每条 response.output_item.done 的 item 规范化哈希 + 终态 response_id。
type openAIWSDeltaRecorder struct {
	mu               sync.Mutex
	itemsAddedHashes []string
	responseID       string
	completed        bool
}

// observe 处理一条上游事件原始字节。
func (r *openAIWSDeltaRecorder) observe(message []byte) {
	if r == nil || len(message) == 0 {
		return
	}
	eventType := gjson.GetBytes(message, "type").String()
	switch eventType {
	case "response.output_item.done":
		item := gjson.GetBytes(message, "item")
		if !item.Exists() {
			return
		}
		r.mu.Lock()
		r.itemsAddedHashes = append(r.itemsAddedHashes, openAIWSDeltaItemHash(json.RawMessage(item.Raw)))
		r.mu.Unlock()
	case "response.completed":
		id := gjson.GetBytes(message, "response.id").String()
		if id == "" {
			id = gjson.GetBytes(message, "response_id").String()
		}
		r.mu.Lock()
		if id != "" {
			r.responseID = id
		}
		r.completed = true
		r.mu.Unlock()
	}
}

// finalize 用当前请求的逻辑 input 哈希与本回合收集到的 output item 哈希
// 生成下一回合可用的链快照。未完成响应不更新链。
func (r *openAIWSDeltaRecorder) finalize(accountID int64, fieldsFP string, logicalInputHashes []string, prev *openAIWSDeltaChainState) (*openAIWSDeltaChainState, bool) {
	if r == nil {
		return prev, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.completed || r.responseID == "" {
		return prev, false
	}
	baseline := make([]string, 0, len(logicalInputHashes)+len(r.itemsAddedHashes))
	baseline = append(baseline, logicalInputHashes...)
	baseline = append(baseline, r.itemsAddedHashes...)
	if len(baseline) > openAIWSDeltaChainMaxBaseline {
		baseline = baseline[:openAIWSDeltaChainMaxBaseline]
	}
	return &openAIWSDeltaChainState{
		ResponseID:      r.responseID,
		AccountID:       accountID,
		FieldsFP:        fieldsFP,
		BaselineHashes:  baseline,
		UpdatedUnixNano: time.Now().UnixNano(),
	}, true
}

// openAIWSExtractRawInputItems 从原始请求体拆出 input 各项的原始字节，
// 保留客户端序列化形态供增量 wire 输出。非数组形态按单元素处理。
func openAIWSExtractRawInputItems(body []byte) []json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return nil
	}
	if input.IsArray() {
		elems := input.Array()
		items := make([]json.RawMessage, 0, len(elems))
		for _, elem := range elems {
			items = append(items, openAIWSRawMessageFromResult(body, elem))
		}
		return items
	}
	return []json.RawMessage{openAIWSRawMessageFromResult(body, input)}
}

// openAIWSDeltaWireItems 校验原始字节项与最终 payload input 是否仍一一对应；
// 对应时返回原始字节（保真），否则对 payload 项重新编码。
func openAIWSDeltaWireItems(body []byte, payloadInput any) []json.RawMessage {
	var count int
	switch items := payloadInput.(type) {
	case []any:
		count = len(items)
	case []json.RawMessage:
		count = len(items)
	default:
		count = 0
	}
	raw := openAIWSExtractRawInputItems(body)
	if count == 0 || len(raw) == count {
		return raw
	}
	// 变换过的 input（replay/截断等）：按 payload 重新编码，功能正确优先。
	switch items := payloadInput.(type) {
	case []json.RawMessage:
		return items
	case []any:
		encoded := make([]json.RawMessage, 0, len(items))
		for _, item := range items {
			if rawItem, ok := item.(json.RawMessage); ok {
				encoded = append(encoded, rawItem)
				continue
			}
			rawItem, err := marshalOpenAIUpstreamJSON(item)
			if err != nil {
				return nil
			}
			encoded = append(encoded, rawItem)
		}
		return encoded
	default:
		if payloadInput == nil {
			return nil
		}
		rawItem, err := marshalOpenAIUpstreamJSON(payloadInput)
		if err != nil {
			return nil
		}
		return []json.RawMessage{rawItem}
	}
}
