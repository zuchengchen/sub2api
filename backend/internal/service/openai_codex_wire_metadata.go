package service

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyCodexWireLaneMetadata 在 codex lane 上把 client_metadata / turn-metadata
// 补齐到真实 Codex CLI 的字段全集（身份字段已由
// ensureCodexClientMetadataDefaults 负责）。kind ∈ {"turn","prewarm"}：
// prewarm 帧的官方画像是 request_kind=prewarm、turn_id 为空串、且没有
// root_turn_id/turn_trigger/workspaces/turn_started_at_unix_ms。
func applyCodexWireLaneMetadata(payload map[string]any, wsHeaders http.Header, account *Account, kind string) {
	if payload == nil || account == nil {
		return
	}
	cm, ok := payload["client_metadata"].(map[string]any)
	if !ok {
		cm = map[string]any{}
		payload["client_metadata"] = cm
	}

	metadata := map[string]any{}
	if raw, ok := cm[openAIWSTurnMetadataHeader].(string); ok && strings.TrimSpace(raw) != "" {
		if decoded, err := decodeCodexTurnMetadata(raw); err == nil && decoded != nil {
			metadata = decoded
		}
	}

	setStr := func(m map[string]any, key, value string) {
		if current, exists := m[key]; exists {
			if s, ok := current.(string); ok && s != "" {
				return
			}
		}
		m[key] = value
	}
	setBool := func(m map[string]any, key string, value bool) {
		if _, exists := m[key]; !exists {
			m[key] = value
		}
	}

	// 行为字段（官方 CodexTurnMetadataPayload 非身份部分）。
	profile, hasProfile := codexDeviceProfileForRequest(account)
	setStr(metadata, "agent_name", "/root")
	setStr(metadata, "history_initialization", "new")
	setStr(metadata, "thread_source", "user")
	sandbox := codexDefaultSandbox
	sandboxMode := codexDefaultWorkspaceMode
	cwd := codexDefaultWorkspaceCWD
	if hasProfile {
		sandbox = profile.Sandbox
		sandboxMode = profile.SandboxMode
		cwd = profile.WorkspaceCWD
	}
	setStr(metadata, "sandbox", sandbox)
	setStr(metadata, "sandbox_mode", sandboxMode)
	setBool(metadata, "auto_review_enabled", false)
	setBool(metadata, "node_repl_auto_review_required", codexAccountNodeReplAutoReviewRequired(account))
	setBool(metadata, "node_repl_disabled", false)
	setBool(metadata, "analytics_enabled", true)
	setStr(metadata, "model", openAIWSPayloadString(payload, "model"))
	if _, exists := metadata["reasoning_effort"]; !exists {
		if effort := strings.TrimSpace(gjson.GetBytes(payloadAsJSONBytes(payload), "reasoning.effort").String()); effort != "" {
			metadata["reasoning_effort"] = effort
		}
	}

	switch kind {
	case "prewarm":
		metadata["request_kind"] = "prewarm"
		// 官方 prewarm 的 turn_id 存在但为空串；turn 专属键整体缺席。
		metadata["turn_id"] = ""
		delete(metadata, "root_turn_id")
		delete(metadata, "turn_trigger")
		delete(metadata, "workspaces")
		delete(metadata, "turn_started_at_unix_ms")
		delete(metadata, "history_ingest_requested")
		delete(metadata, "compaction")
	default: // "turn"
		if _, exists := metadata["request_kind"]; !exists {
			metadata["request_kind"] = "turn"
		}
		// 官方 root_turn_id：新线程首回合时等于 turn_id（抓包 frame1 证实两值相同）。
		if _, exists := metadata["root_turn_id"]; !exists {
			if turnID, ok := metadata["turn_id"].(string); ok && turnID != "" {
				metadata["root_turn_id"] = turnID
			}
		}
		setStr(metadata, "turn_trigger", codexWireAccountExtra(account, "codex_turn_trigger", "user"))
		if _, exists := metadata["workspaces"]; !exists {
			metadata["workspaces"] = map[string]any{cwd: map[string]any{}}
		}
		if _, exists := metadata["turn_started_at_unix_ms"]; !exists {
			metadata["turn_started_at_unix_ms"] = time.Now().UnixMilli()
		}
	}

	if raw, err := marshalCodexTurnMetadata(metadata); err == nil {
		cm[openAIWSTurnMetadataHeader] = string(raw)
		if wsHeaders != nil {
			// 握手头的 turn-metadata 与官方一致地裁掉 tool_namespaces_info。
			if headerRaw, err := sjson.DeleteBytes(raw, "tool_namespaces_info"); err == nil {
				wsHeaders.Set(openAIWSTurnMetadataHeader, string(headerRaw))
			}
		}
	}
	// 平铺身份键与 metadata 同步（官方 client_metadata 同时携带两份投影）。
	flatPairs := [][2]string{
		{"session_id", "session_id"},
		{"thread_id", "thread_id"},
		{"turn_id", "turn_id"},
		{"x-codex-window-id", "window_id"},
		{"x-codex-installation-id", "installation_id"},
	}
	if kind == "turn" {
		// 官方 turn 帧在 client_metadata 平铺层也带 root_turn_id（与 md 同值）；
		// prewarm 画像不带。
		flatPairs = append(flatPairs, [2]string{"root_turn_id", "root_turn_id"})
	}
	for _, pair := range flatPairs {
		if v, ok := metadata[pair[1]].(string); ok {
			cm[pair[0]] = v
		}
	}

	// 官方 client_metadata 里的工具/额度/lite 标记。
	if _, exists := cm["mcp_attribution"]; !exists {
		cm["mcp_attribution"] = `{"status":"none"}`
	}
	if _, exists := cm["guardian_credits_requested"]; !exists {
		cm["guardian_credits_requested"] = "true"
	}
	// 官方 use_responses_lite 是模型级属性（models-manager catalog）：lite
	// 标记与 reasoning.context=all_turns 必须同现，上游强制配对。网关感知不到
	// 模型目录，默认以请求是否已带 context=all_turns 近似；账号 extra
	// codex_responses_lite=true 强制开启（缺 context 时补齐）、=false 关闭。
	liteCtx := strings.EqualFold(
		gjson.GetBytes(payloadAsJSONBytes(payload), "reasoning.context").String(), "all_turns")
	switch codexWireAccountExtra(account, "codex_responses_lite", "") {
	case "true":
		if !liteCtx {
			if r, ok := payload["reasoning"].(map[string]any); ok {
				r["context"] = "all_turns"
			} else {
				payload["reasoning"] = map[string]any{"context": "all_turns"}
			}
		}
		if _, exists := cm["ws_request_header_x_openai_internal_codex_responses_lite"]; !exists {
			cm["ws_request_header_x_openai_internal_codex_responses_lite"] = "true"
		}
	case "false":
	default:
		if liteCtx {
			if _, exists := cm["ws_request_header_x_openai_internal_codex_responses_lite"]; !exists {
				cm["ws_request_header_x_openai_internal_codex_responses_lite"] = "true"
			}
		}
	}
	if _, exists := cm["x-codex-ws-stream-request-start-ms"]; !exists {
		cm["x-codex-ws-stream-request-start-ms"] = strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
}

// relocateCodexWSTurnState 把 x-codex-turn-state 从握手头挪进
// client_metadata——真实客户端不在 WS 握手上发该头，门票值随帧传输。
func relocateCodexWSTurnState(wsHeaders http.Header, payload map[string]any) {
	if wsHeaders == nil || payload == nil {
		return
	}
	state := strings.TrimSpace(wsHeaders.Get(openAIWSTurnStateHeader))
	if state == "" {
		return
	}
	wsHeaders.Del(openAIWSTurnStateHeader)
	cm, ok := payload["client_metadata"].(map[string]any)
	if !ok {
		cm = map[string]any{}
		payload["client_metadata"] = cm
	}
	if _, exists := cm[openAIWSTurnStateHeader]; !exists {
		cm[openAIWSTurnStateHeader] = state
	}
}

func codexWireAccountExtra(account *Account, key, fallback string) string {
	if account == nil {
		return fallback
	}
	if v := strings.TrimSpace(account.GetExtraString(key)); v != "" {
		return v
	}
	return fallback
}
