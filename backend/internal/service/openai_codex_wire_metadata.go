package service

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// HTTP and WebSocket share logical turn metadata. Transport-only fields are
// added by the corresponding projection, without inventing client capabilities.
func applyCodexWireLaneMetadata(payload map[string]any, wsHeaders http.Header, account *Account, kind string) {
	applyCodexWireMetadata(payload, wsHeaders, account, kind, true)
}

func applyCodexWireMetadata(payload map[string]any, wsHeaders http.Header, account *Account, kind string, websocket bool) {
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
		if strings.TrimSpace(value) == "" {
			return
		}
		if current, exists := m[key]; exists {
			if s, ok := current.(string); ok && s != "" {
				return
			}
		}
		m[key] = value
	}
	// Flat metadata and projected headers can be the only available ancestry.
	// Keep those already-isolated references before considering root defaults.
	for _, field := range []string{"parent_thread_id", "forked_from_thread_id", "parent_turn_id", "root_turn_id"} {
		setStr(metadata, field, firstNonEmptyString(cm[field]))
	}
	setStr(metadata, "parent_thread_id", firstNonEmptyString(cm["x-codex-parent-thread-id"], wsHeaders.Get("x-codex-parent-thread-id")))
	// Environment and capability claims require explicit configuration. Preserve
	// native client metadata instead of replacing it with the gateway's host.
	profile, hasProfile := codexDeviceProfileForRequest(account)
	if hasProfile {
		if account.GetExtraString(codexSandboxExtraKey) != "" {
			setStr(metadata, "sandbox", profile.Sandbox)
		}
		if account.GetExtraString(codexSandboxModeExtraKey) != "" {
			setStr(metadata, "sandbox_mode", profile.SandboxMode)
		}
	}
	if model := openAIWSPayloadString(payload, "model"); model != "" {
		metadata["model"] = model
	}
	if effort := strings.TrimSpace(gjson.GetBytes(payloadAsJSONBytes(payload), "reasoning.effort").String()); effort != "" {
		metadata["reasoning_effort"] = effort
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
		if _, exists := metadata["root_turn_id"]; !exists && firstNonEmptyString(metadata["parent_thread_id"], metadata["parent_turn_id"], cm["x-openai-subagent"], wsHeaders.Get("x-openai-subagent")) == "" {
			if turnID, ok := metadata["turn_id"].(string); ok && turnID != "" {
				metadata["root_turn_id"] = turnID
			}
		}
		if trigger := account.GetExtraString("codex_turn_trigger"); trigger != "" {
			setStr(metadata, "turn_trigger", trigger)
		}
		if _, exists := metadata["workspaces"]; !exists && hasProfile && account.GetExtraString(codexWorkspaceCWDExtraKey) != "" {
			metadata["workspaces"] = map[string]any{profile.WorkspaceCWD: map[string]any{}}
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

	if !websocket {
		return
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

// applyCodexHTTPWireMetadata edits only the small metadata object. Large native
// inputs, tool schemas and their integer values keep their original bytes.
func applyCodexHTTPWireMetadata(req *http.Request, body []byte, account *Account) ([]byte, error) {
	if req == nil || account == nil || !account.IsOpenAI() || !account.IsOpenAIOAuthLike() {
		return body, nil
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return body, nil
	}
	cm := map[string]any{}
	if raw := root.Get("client_metadata"); raw.IsObject() {
		if err := decodeCodexMetadataObject(raw.Raw, &cm); err != nil {
			return body, err
		}
	} else if raw.Exists() {
		return body, nil
	}
	payload := map[string]any{"client_metadata": cm, "model": root.Get("model").String()}
	if reasoning := root.Get("reasoning"); reasoning.Exists() {
		payload["reasoning"] = json.RawMessage(reasoning.Raw)
	}
	applyCodexWireMetadata(payload, req.Header, account, "turn", false)
	raw, err := marshalOpenAIUpstreamJSON(cm)
	if err != nil {
		return body, err
	}
	next, err := sjson.SetRawBytes(body, "client_metadata", raw)
	if err != nil {
		return body, err
	}
	if bytes.Equal(next, body) {
		return body, nil
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewReader(next))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(next)), nil }
	req.ContentLength = int64(len(next))
	return next, nil
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
