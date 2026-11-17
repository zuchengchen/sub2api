package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIResponsesNamespaceNamesContextKey = "openai_responses_namespace_names"

// shouldFlattenOpenAIResponsesNamespaces 判定原生 Responses 转发前是否摊平
// Codex namespace 工具。
//
// 默认不摊平：OAuth 账号的 HTTP 出口恒为 chatgpt.com/backend-api/codex/responses
// （buildUpstreamRequest 只在 API Key 分支读 base_url），也就是 namespace 扩展的
// 定义方本身；Codex 客户端对 WS 与 HTTP 两条传输发送同一份 tools（codex-rs
// client.rs build_responses_request 无传输分支，WS 失败后会 session 级回落 HTTP
// 继续发同样的声明）。摊平只改写工具名，改不掉客户端在 tools 描述与 developer
// 消息里写死的 `to=functions.<namespace>.<tool>` 寻址约定，模型据此寻址必然落空
// （issue #4978）；命名空间名还可由 features.multi_agent_v2.tool_namespace 自定义、
// 或由 MCP/connector 动态生成（mcp__codex_apps__gmail），保留名单枚举不完。
//
// compact 端点例外：已知它的 schema 比 /responses 窄（连 input[].namespace 都会报
// Unknown parameter，见 issue #4761），而是否接受 namespace 工具声明没有任何实测
// 证据；compact 只做历史摘要、不需要模型寻址工具，回程也没有工具调用可还原，因此
// 保持 0.1.166 起就在跑的摊平行为，不随本次默认值翻转扩大风险面。
//
// 账号开关 openai_responses_flatten_namespaces 为不认识 namespace 的兼容上游保留
// 退路，打开后恢复旧行为：WSv2 上游原生支持 namespace，且 WS 出口
// （openai_ws_forwarder_v2）原样转发上游事件、不经 HTTP 回程还原，摊平后的平名
// 无法还原会破坏客户端工具匹配，因此实际走 WSv2 分支的请求仍保持 namespace 原样；
// 透传账号先于 WSv2 分支经 HTTP 转发返回，仍需摊平。
func shouldFlattenOpenAIResponsesNamespaces(
	account *Account,
	transport OpenAIUpstreamTransport,
	passthroughEnabled bool,
	compactPath bool,
) bool {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return false
	}
	if !compactPath && !account.IsOpenAIResponsesFlattenNamespacesEnabled() {
		return false
	}
	if transport == OpenAIUpstreamTransportResponsesWebsocketV2 && !passthroughEnabled {
		return false
	}
	return true
}

// shouldStripOpenAIResponsesInputNamespaces removes residual input item
// namespaces for OpenAI OAuth and API Key HTTP forwarding. Native WSv2 keeps
// namespaces because that protocol supports them and does not restore payloads.
func shouldStripOpenAIResponsesInputNamespaces(account *Account, transport OpenAIUpstreamTransport, passthroughEnabled bool) bool {
	if account == nil || (!account.IsOpenAIOAuthLike() && !account.IsOpenAIApiKey()) {
		return false
	}
	if transport == OpenAIUpstreamTransportResponsesWebsocketV2 && !passthroughEnabled {
		return false
	}
	return true
}

// shouldKeepOpenAIResponsesToolCallNamespaces 判定清理 input 残留 namespace 时是否
// 保留工具调用项上的 namespace。
//
// 上游对这个字段有两套互斥要求，判定按「出口 + 端点」而非工具声明内容：
//   - /backend-api/codex/responses 会按 namespace 解析历史调用，缺字段直接 400
//     `Missing namespace for function_call '...'. Round-trip the model's
//     function_call item with its namespace field included.`（issue #4761 回帖），
//     故 OAuth 非 compact 请求必须保留。
//   - compact 端点的 schema 不含该字段，携带即 400 `Unknown parameter:
//     input[N].namespace`（issue #4761 正文），故 compact 一律清理。
//   - API Key 原生 Responses 保留调用/函数结果的 namespace。历史调用的工具
//     不一定仍在本轮 tools 声明中；声明缺席不能证明该身份字段可删除。
//   - 摊平只清理实际映射的调用及其唯一匹配的结果；保留的声明、未映射历史
//     和未知类型仍需携带原来的 namespace。
func shouldKeepOpenAIResponsesToolCallNamespaces(
	account *Account,
	transport OpenAIUpstreamTransport,
	passthroughEnabled bool,
	compactPath bool,
	body []byte,
) bool {
	if account == nil {
		return false
	}
	if compactPath {
		return false
	}
	if account.IsOpenAIApiKey() {
		return true
	}
	if !account.IsOpenAIOAuthLike() {
		return false
	}
	return true
}

func hasOpenAIResponsesNamespaceToolDeclaration(body []byte) bool {
	hasNamespaceTool := func(tools gjson.Result) bool {
		if !tools.IsArray() {
			return false
		}
		found := false
		tools.ForEach(func(_, tool gjson.Result) bool {
			if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "namespace") {
				found = true
				return false
			}
			return true
		})
		return found
	}

	if hasNamespaceTool(gjson.GetBytes(body, "tools")) {
		return true
	}

	// Responses Lite moves private namespace declarations out of top-level
	// tools and into an input.additional_tools carrier. API-key requests are
	// intentionally not rewritten by normalizeOpenAIResponsesLiteTools, so the
	// declaration can arrive here only in this form.
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		if !strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "additional_tools") {
			return true
		}
		if hasNamespaceTool(item.Get("tools")) {
			found = true
			return false
		}
		return true
	})
	return found
}

// openAIResponsesToolCallItemTypes keeps the existing targeted namespace-retry
// allowlist. Native FunctionCallOutput also carries namespace, but preserving
// its identity does not expand the retry allowlist for deleting that field.
var openAIResponsesToolCallItemTypes = map[string]bool{
	"function_call":    true,
	"tool_call":        true,
	"custom_tool_call": true,
	"mcp_tool_call":    true,
}

func isOpenAIResponsesToolCallItemType(itemType string) bool {
	return openAIResponsesToolCallItemTypes[strings.ToLower(strings.TrimSpace(itemType))]
}

func isOpenAIResponsesNamespaceContextItemType(itemType string) bool {
	// An unfamiliar item may carry native tool identity. Only known passive
	// families authorize proactive cleanup; declining replay is not permission
	// to delete an unknown family's fields. Compact still strips unconditionally.
	return !isCodexKnownNonToolCallItemType(strings.ToLower(strings.TrimSpace(itemType)))
}

func flattenOpenAIResponsesNamespaces(c *gin.Context, body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"namespace"`)) {
		return body, nil
	}
	var requestBody map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &requestBody); err != nil {
		return body, fmt.Errorf("decode OpenAI namespace body: %w", err)
	}
	if err := validateOpenAINamespaceFlattenContract(requestBody); err != nil {
		return body, err
	}
	names, changed, err := apicompat.FlattenResponsesNamespacesExcept(requestBody, map[string]bool{"image_gen": true})
	if err != nil {
		return body, err
	}
	if !changed {
		return body, nil
	}
	rewriteOpenAIResponsesFlattenedOutputs(body, requestBody, names)
	rebuilt, err := marshalOpenAIUpstreamJSON(requestBody)
	if err != nil {
		return body, fmt.Errorf("encode OpenAI namespace body: %w", err)
	}
	setOpenAIResponsesNamespaceNames(c, names)
	return restoreOpenAIUpstreamJSONTopLevelOrder(body, rebuilt), nil
}

// rewriteOpenAIResponsesFlattenedOutputs follows only a unique inline call that
// the namespace converter actually rewrote. A declaration alone cannot prove
// the identity of an output whose call belongs to opaque earlier context.
func rewriteOpenAIResponsesFlattenedOutputs(original []byte, body map[string]any, names map[string]apicompat.ResponsesNamespaceName) {
	originalInput := gjson.GetBytes(original, "input")
	input, ok := body["input"].([]any)
	if !ok || !originalInput.IsArray() || len(names) == 0 {
		return
	}
	type mappedCall struct {
		namespace string
		name      string
		flatName  string
		index     int
	}
	calls := make(map[string]mappedCall)
	counts := make(map[string]int)
	originalInput.ForEach(func(index, item gjson.Result) bool {
		callID := item.Get("call_id")
		if !item.IsObject() || callID.Type != gjson.String || callID.String() == "" || item.Get("type").String() == "function_call_output" {
			return true
		}
		id := callID.String()
		counts[id]++
		i := int(index.Int())
		if item.Get("type").String() != "function_call" || i < 0 || i >= len(input) {
			return true
		}
		rewritten, ok := input[i].(map[string]any)
		if !ok {
			return true
		}
		flat, _ := rewritten["name"].(string)
		identity, mapped := names[flat]
		_, stillNamespaced := rewritten["namespace"]
		if !mapped || stillNamespaced || item.Get("namespace").Type != gjson.String || item.Get("name").Type != gjson.String ||
			identity.Namespace != item.Get("namespace").String() || identity.Name != item.Get("name").String() {
			return true
		}
		calls[id] = mappedCall{namespace: identity.Namespace, name: identity.Name, flatName: flat, index: i}
		return true
	})
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "function_call_output" {
			continue
		}
		callID, _ := item["call_id"].(string)
		call, mapped := calls[callID]
		rawNamespace, namespaceExists := item["namespace"]
		namespace, namespaceOK := rawNamespace.(string)
		if !mapped || counts[callID] != 1 || call.index >= i || (namespaceExists && (!namespaceOK || namespace != call.namespace)) {
			continue
		}
		if rawName, exists := item["name"]; exists {
			name, ok := rawName.(string)
			if !ok || name != call.name {
				continue
			}
			item["name"] = call.flatName
		}
		delete(item, "namespace")
	}
}

// FlattenResponsesNamespacesExcept handles function children. A native custom
// child has a different input/output contract, so it must never disappear as a
// side effect of flattening another function. Excluded namespaces stay native.
func validateOpenAINamespaceFlattenContract(body map[string]any) error {
	tools, _ := body["tools"].([]any)
	willFlatten := false
	var ambiguous *apicompat.RequestContractError
	var unsupported *apicompat.RequestContractError
	for index, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		toolType, _ := tool["type"].(string)
		if strings.TrimSpace(toolType) != "namespace" {
			continue
		}
		name, _ := tool["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" || name == "image_gen" {
			continue
		}
		children, _ := tool["tools"].([]any)
		alternate, _ := tool["children"].([]any)
		if len(children) > 0 && len(alternate) > 0 && ambiguous == nil {
			ambiguous = &apicompat.RequestContractError{Param: fmt.Sprintf("tools[%d]", index), Message: "A namespace cannot be flattened with both nonempty tools and children declarations"}
		}
		field := "tools"
		if len(children) == 0 {
			children, field = alternate, "children"
		}
		for childIndex, rawChild := range children {
			child, ok := rawChild.(map[string]any)
			childType, _ := child["type"].(string)
			if ok && strings.TrimSpace(childType) == "function" {
				childName, _ := child["name"].(string)
				willFlatten = willFlatten || strings.TrimSpace(childName) != ""
				continue
			}
			if unsupported == nil {
				unsupported = &apicompat.RequestContractError{Param: fmt.Sprintf("tools[%d].%s[%d].type", index, field, childIndex), Message: "Namespace flattening cannot preserve this explicit tool type; disable namespace flattening to retain the native declaration"}
			}
		}
	}
	if willFlatten && ambiguous != nil {
		return ambiguous
	}
	if willFlatten && unsupported != nil {
		return unsupported
	}
	return nil
}

// stripOpenAIResponsesInputNamespaces removes namespace only from direct input
// array items. Namespace declarations and nested namespace fields are left
// untouched. Rebuilding the input array once keeps this linear for long
// histories and avoids decoding JSON numbers through float64.
//
// keepToolCallNamespaces 保留工具调用项（function_call / custom_tool_call 等）上的
// namespace，让 Codex 调用能按上游要求原样回传；判定见
// shouldKeepOpenAIResponsesToolCallNamespaces。
func stripOpenAIResponsesInputNamespaces(body []byte, keepToolCallNamespaces bool) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"namespace"`)) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}

	var rebuilt bytes.Buffer
	changed := false
	// Offsets below are relative to input.Raw, not the full request body.
	copyFrom := 0
	var stripErr error
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() || !item.Get("namespace").Exists() ||
			(keepToolCallNamespaces && isOpenAIResponsesNamespaceContextItemType(item.Get("type").String())) {
			return true
		}
		itemBody, err := sjson.DeleteBytes([]byte(item.Raw), "namespace")
		if err != nil {
			stripErr = err
			return false
		}
		if !changed {
			// Allocate only on the first change; copy unchanged spans without
			// traversing their items again or converting them to byte slices.
			rebuilt.Grow(len(input.Raw))
			changed = true
		}
		itemStart := item.Index - input.Index
		_, _ = rebuilt.WriteString(input.Raw[copyFrom:itemStart])
		_, _ = rebuilt.Write(itemBody)
		copyFrom = itemStart + len(item.Raw)
		return true
	})
	if stripErr != nil {
		return body, fmt.Errorf("delete OpenAI input namespace: %w", stripErr)
	}
	if !changed {
		return body, nil
	}
	_, _ = rebuilt.WriteString(input.Raw[copyFrom:])
	stripped, err := sjson.SetRawBytes(body, "input", rebuilt.Bytes())
	if err != nil {
		return body, fmt.Errorf("replace OpenAI input after namespace deletion: %w", err)
	}
	return stripped, nil
}

func setOpenAIResponsesNamespaceNames(c *gin.Context, names map[string]apicompat.ResponsesNamespaceName) {
	if c != nil && len(names) > 0 {
		c.Set(openAIResponsesNamespaceNamesContextKey, names)
	}
}

// clearOpenAIResponsesNamespaceNames 清除上一次尝试登记的摊平名映射。handler 的
// failover 会在同一个 *gin.Context 上重试下一个账号，映射不清会让保留 namespace 的
// 账号拿着上一个账号的摊平名做回程还原。
func clearOpenAIResponsesNamespaceNames(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get(openAIResponsesNamespaceNamesContextKey); exists {
		c.Set(openAIResponsesNamespaceNamesContextKey, map[string]apicompat.ResponsesNamespaceName(nil))
	}
}

func openAIResponsesNamespaceNames(c *gin.Context) map[string]apicompat.ResponsesNamespaceName {
	if c == nil {
		return nil
	}
	value, ok := c.Get(openAIResponsesNamespaceNamesContextKey)
	if !ok {
		return nil
	}
	names, _ := value.(map[string]apicompat.ResponsesNamespaceName)
	return names
}

func restoreOpenAIResponsesNamespacePayload(c *gin.Context, payload []byte) ([]byte, error) {
	names := openAIResponsesNamespaceNames(c)
	if len(names) == 0 || !json.Valid(payload) {
		return payload, nil
	}
	restored, changed, err := apicompat.RestoreResponsesNamespaceCalls(payload, names)
	if err != nil {
		return payload, err
	}
	if changed {
		return restored, nil
	}
	return payload, nil
}
