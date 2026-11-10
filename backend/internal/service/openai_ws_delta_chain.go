package service

import (
	"bytes"
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
	ResponseID      string                       `json:"response_id"`
	AccountID       int64                        `json:"account_id"`
	FieldsFP        string                       `json:"fields_fp"`
	BaselineHashes  []string                     `json:"baseline"`
	UpdatedUnixNano int64                        `json:"updated_unix_nano"`
	Scope           string                       `json:"-"`
	WindowID        string                       `json:"-"`
	PendingTools    map[[sha256.Size]byte]string `json:"-"`
}

const openAIWSDeltaChainMaxBaseline = 2048
const openAIWSDeltaCheckpointTTL = 10 * time.Minute

// openAIWSDeltaItemHash 对单个 input/output item 规范化后取指纹，
// 客户端对历史项的再序列化差异（键序、空白）被消掉。
func openAIWSDeltaItemHash(item json.RawMessage) string {
	sum, ok := openAIWSContinuationItemDigest(item)
	if !ok {
		sum = sha256.Sum256(normalizeOpenAIWSJSONForCompareOrRaw(item))
	}
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
// include, service_tier, prompt_cache_key, prompt_cache_options, text；
// 忽略 input, previous_response_id, stream_options, client_metadata,
// access_programs（以及 enum tag type 与网关字段）。
func openAIWSDeltaFieldsFingerprint(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	props := make(map[string]any, len(payload))
	for key, value := range payload {
		switch key {
		case "input", "previous_response_id", "stream_options", "client_metadata", "access_programs", "type", "background":
			continue
		case "generate":
			if generate, ok := value.(bool); ok && !generate {
				continue // Official prewarm and generation share the same context.
			}
		}
		props[key] = value
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
	return resolveOpenAIWSDeltaWithHashes(chain, accountID, fieldsFP, currentItems, nil)
}

func resolveOpenAIWSDeltaWithHashes(chain *openAIWSDeltaChainState, accountID int64, fieldsFP string, currentItems []json.RawMessage, hashes []string) openAIWSDeltaDecision {
	if chain == nil {
		return openAIWSDeltaDecision{Reason: "no_chain"}
	}
	if len(chain.BaselineHashes) > openAIWSDeltaChainMaxBaseline || len(currentItems) > openAIWSDeltaChainMaxBaseline {
		return openAIWSDeltaDecision{Reason: "baseline_too_large"}
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
		currentHash := ""
		if len(hashes) == len(currentItems) {
			currentHash = hashes[idx]
		} else {
			currentHash = openAIWSDeltaItemHash(currentItems[idx])
		}
		if currentHash != hash {
			return openAIWSDeltaDecision{Reason: "input_mismatch"}
		}
	}
	if hashes == nil {
		if reason := openAIWSDeltaInputIneligible(currentItems); reason != "" {
			return openAIWSDeltaDecision{Reason: reason}
		}
	}
	if !openAIWSContinuationToolsComplete(openAIWSDeltaJSONItems(currentItems[len(chain.BaselineHashes):]), chain.PendingTools) {
		return openAIWSDeltaDecision{Reason: "incomplete_tool_delta"}
	}
	return openAIWSDeltaDecision{
		Delta:            true,
		TailItems:        append([]json.RawMessage{}, currentItems[len(chain.BaselineHashes):]...),
		PreviousResponse: chain.ResponseID,
		Reason:           "incremental",
	}
}

// openAIWSDeltaRecorder 在一次响应流上收集链所需的增量信息：
// 每条 response.output_item.done 的 item 规范化哈希 + 终态 response_id。
type openAIWSDeltaRecorder struct {
	mu               sync.Mutex
	output           openAIWSContinuationOutputCollector
	itemsAddedHashes []string
	pendingTools     map[[sha256.Size]byte]string
	responseID       string
	completed        bool
	overflow         bool
}

// Observe raw upstream output before model/tool rewrites. Only successful,
// unambiguous terminal output can establish the next socket checkpoint.
func (r *openAIWSDeltaRecorder) observe(message []byte) {
	if r == nil || len(message) == 0 {
		return
	}
	eventType := gjson.GetBytes(message, "type").String()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.output.observe(message, eventType)
	if r.output.invalid {
		r.overflow = true
	}
	if eventType != "response.completed" {
		return
	}
	response := gjson.GetBytes(message, "response")
	if status := response.Get("status").String(); (status != "" && status != "completed") ||
		response.Get("error").Type == gjson.JSON || gjson.GetBytes(message, "error").Type == gjson.JSON {
		r.overflow = true
		return
	}
	output, ok := r.output.finish(response.Get("output"))
	if !ok {
		r.overflow = true
		return
	}
	r.responseID = strings.Clone(strings.TrimSpace(response.Get("id").String()))
	if r.responseID == "" {
		r.responseID = strings.Clone(strings.TrimSpace(gjson.GetBytes(message, "response_id").String()))
	}
	if r.responseID == "" || len(r.responseID) > 256 {
		r.overflow = true
		return
	}
	r.pendingTools = make(map[[sha256.Size]byte]string)
	for _, item := range output {
		if item.toolType != "" {
			if _, duplicate := r.pendingTools[item.callID]; duplicate {
				r.overflow = true
				return
			}
			r.pendingTools[item.callID] = item.toolType
		}
		r.itemsAddedHashes = append(r.itemsAddedHashes, hex.EncodeToString(item.digest[:]))
	}
	r.completed = true
}

// nil,true invalidates an unusable old session hint. An incomplete stream can
// never publish a new checkpoint, even when it carried some completed items.
func (r *openAIWSDeltaRecorder) finalize(accountID int64, fieldsFP string, logicalInputHashes []string, prev *openAIWSDeltaChainState) (*openAIWSDeltaChainState, bool) {
	if r == nil {
		return prev, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overflow || len(logicalInputHashes)+len(r.output.items) > openAIWSDeltaChainMaxBaseline || len(logicalInputHashes)+len(r.itemsAddedHashes) > openAIWSDeltaChainMaxBaseline {
		return nil, true
	}
	if !r.completed || r.responseID == "" {
		return prev, false
	}
	baseline := make([]string, 0, len(logicalInputHashes)+len(r.itemsAddedHashes))
	baseline = append(baseline, logicalInputHashes...)
	baseline = append(baseline, r.itemsAddedHashes...)
	return &openAIWSDeltaChainState{
		ResponseID: r.responseID, AccountID: accountID, FieldsFP: fieldsFP,
		BaselineHashes: baseline, UpdatedUnixNano: time.Now().UnixNano(), PendingTools: r.pendingTools,
	}, true
}

func openAIWSDeltaJSONItems(items []json.RawMessage) []gjson.Result {
	parsed := make([]gjson.Result, len(items))
	for i, item := range items {
		parsed[i] = gjson.ParseBytes(item)
	}
	return parsed
}

// These checks only gate gateway-inferred continuation. They never drop or
// alter user history, and native clients still own explicitly supplied anchors.
func openAIWSDeltaInputIneligible(items []json.RawMessage) string {
	_, reason := openAIWSDeltaInputSnapshot(items)
	return reason
}

func openAIWSDeltaInputSnapshot(items []json.RawMessage) ([]string, string) {
	hashes := make([]string, 0, len(items))
	for _, item := range items {
		if gjson.GetBytes(item, "type").String() == "compaction_trigger" {
			return nil, "compaction_boundary"
		}
		digest, ok := openAIWSContinuationItemDigest(item)
		if !ok {
			return nil, "non_self_contained_input"
		}
		hashes = append(hashes, hex.EncodeToString(digest[:]))
	}
	if !openAIWSContinuationToolsComplete(openAIWSDeltaJSONItems(items), nil) {
		return nil, "incomplete_tool_pairs"
	}
	return hashes, ""
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
	if len(raw) == count && count > 0 {
		encoded, err := marshalOpenAIUpstreamJSON(payloadInput)
		if err == nil {
			values := gjson.ParseBytes(encoded).Array()
			match := len(values) == len(raw)
			for i := 0; match && i < len(values); i++ {
				match = bytes.Equal(normalizeOpenAIWSJSONForCompareOrRaw(raw[i]), normalizeOpenAIWSJSONForCompareOrRaw([]byte(values[i].Raw)))
			}
			if match {
				return raw
			}
		}
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

// openAIWSResponseProducedOnConn reports whether responseID was produced on
// connID. previous_response_id is only valid on the socket that produced it
// (store=false; the official client resends full input after any reconnect).
func openAIWSResponseProducedOnConn(store OpenAIWSStateStore, responseID, connID string) (string, bool) {
	if store == nil || strings.TrimSpace(responseID) == "" || strings.TrimSpace(connID) == "" {
		return "", false
	}
	producer, ok := store.GetResponseConn(responseID)
	producer = strings.TrimSpace(producer)
	return producer, ok && producer == strings.TrimSpace(connID)
}
