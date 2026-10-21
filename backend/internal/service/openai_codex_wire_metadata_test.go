package service

import (
	"testing"

	"github.com/tidwall/gjson"
)

const codexLiteMarkerKey = "ws_request_header_x_openai_internal_codex_responses_lite"

func liteTestAccount(extra map[string]any) *Account {
	return &Account{ID: 7, Type: AccountTypeOAuth, Platform: PlatformOpenAI, Extra: extra}
}

func liteMarkerValue(t *testing.T, payload map[string]any) bool {
	t.Helper()
	cm, _ := payload["client_metadata"].(map[string]any)
	return cm[codexLiteMarkerKey] == "true"
}

// 默认模式：请求不带 reasoning.context=all_turns 时不发 lite 标记
// （否则上游因 marker/context 不配对接连拒绝，生产冒烟实测）。
func TestCodexWireLiteMarker_OmittedWithoutAllTurns(t *testing.T) {
	payload := map[string]any{"model": "gpt-5.6-luna"}
	applyCodexWireLaneMetadata(payload, nil, liteTestAccount(map[string]any{}), "turn")
	if liteMarkerValue(t, payload) {
		t.Fatalf("lite marker emitted without reasoning.context=all_turns")
	}
}

// 默认模式：请求已带 all_turns 时发 lite 标记（与官方 lite 模型行为一致）。
func TestCodexWireLiteMarker_EmittedWithAllTurns(t *testing.T) {
	payload := map[string]any{
		"model":     "gpt-5.6-luna",
		"reasoning": map[string]any{"effort": "low", "context": "all_turns"},
	}
	applyCodexWireLaneMetadata(payload, nil, liteTestAccount(map[string]any{}), "turn")
	if !liteMarkerValue(t, payload) {
		t.Fatalf("lite marker missing with reasoning.context=all_turns")
	}
}

// extra=false 时即便请求带 all_turns 也不发。
func TestCodexWireLiteMarker_DisabledByAccount(t *testing.T) {
	payload := map[string]any{
		"model":     "gpt-5.6-luna",
		"reasoning": map[string]any{"context": "all_turns"},
	}
	applyCodexWireLaneMetadata(payload, nil, liteTestAccount(map[string]any{"codex_responses_lite": "false"}), "turn")
	if liteMarkerValue(t, payload) {
		t.Fatalf("lite marker emitted despite codex_responses_lite=false")
	}
}

// extra=true 强制开启：缺 reasoning 时补齐 context=all_turns 并下发标记。
func TestCodexWireLiteMarker_ForcedInjectsContext(t *testing.T) {
	payload := map[string]any{"model": "gpt-5.6-luna"}
	applyCodexWireLaneMetadata(payload, nil, liteTestAccount(map[string]any{"codex_responses_lite": "true"}), "turn")
	if !liteMarkerValue(t, payload) {
		t.Fatalf("lite marker missing with codex_responses_lite=true")
	}
	raw, _ := payloadAsJSONBytes(payload), 0
	if got := gjson.GetBytes(raw, "reasoning.context").String(); got != "all_turns" {
		t.Fatalf("reasoning.context not injected: %q", got)
	}
}
