package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestMarshalCodexWSCreateFrame_KeyOrder(t *testing.T) {
	body := []byte(`{"model":"gpt-5","instructions":"hi","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]}],"tools":[{"type":"web_search","filters":{"allowed_domains":["a.com"]}}],"reasoning":{"context":"all_turns","effort":"low"},"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"k"}`)
	payload := map[string]any{
		"type":                "response.create",
		"model":               "gpt-5",
		"instructions":        "hi",
		"stream":              true,
		"input":               []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]}`)},
		"tools":               []any{map[string]any{"type": "web_search"}},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": "low", "context": "all_turns"},
		"store":               false,
		"include":             []any{"reasoning.encrypted_content"},
		"prompt_cache_key":    "k",
		"client_metadata":     map[string]any{"turn_id": "t1", "session_id": "s1", "x-codex-turn-state": "abc"},
	}
	out, err := marshalCodexWSCreateFrame(body, payload, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	// serde 序：type,model,stream,...,previous_response_id(缺),input,tools,tool_choice,
	// parallel_tool_calls,reasoning,store,stream_options,include,prompt_cache_key,
	// text,generate,client_metadata — 直接调用 marshal 时 instructions 落在未知键
	// 区；真实路径在 buildOpenAIWSCreatePayload 已把它下沉为 input developer item。
	wantOrder := []string{
		`"type":"response.create"`,
		`"model":"gpt-5"`,
		`"stream":true`,
		`"input":[`,
		`"tools":[`,
		`"tool_choice"`,
		`"parallel_tool_calls"`,
		`"reasoning":{"effort":"low","context":"all_turns"}`,
		`"store":false`,
		`"include":["reasoning.encrypted_content"]`,
		`"prompt_cache_key":"k"`,
		`"client_metadata":`,
	}
	idx := -1
	for _, frag := range wantOrder {
		pos := strings.Index(s, frag)
		if pos < 0 {
			t.Fatalf("missing fragment %q in %s", frag, s)
		}
		if pos <= idx {
			t.Fatalf("out-of-order fragment %q", frag)
		}
		idx = pos
	}
	// tools 透传原始请求体字节（含 filters 深结构）。
	if !strings.Contains(s, `"filters":{"allowed_domains":["a.com"]}`) {
		t.Fatalf("tools raw passthrough lost: %s", s)
	}
	// client_metadata 键序：x-codex-ws-stream-request-start-ms 开头样本序，
	// x-codex-turn-state 在 thread_id 后、x-codex-turn-metadata 前。
	cmIdx := strings.Index(s, `"client_metadata":`)
	tail := s[cmIdx:]
	ordState := strings.Index(tail, `"x-codex-turn-state"`)
	if ordState < 0 {
		t.Fatalf("turn-state key missing in client_metadata: %s", tail)
	}
}

func TestMarshalCodexWSCreateFrame_RawPassthrough(t *testing.T) {
	// 强类型字段（reasoning/text）按官方 serde 序归一化；未改写且非强类型的
	// 字段（tools）直接引用原始请求体字节。
	body := []byte(`{"model":"m","input":[{"type":"x"}],"reasoning":{"z":1,"a":2},"text":{"format":{"type":"text"},"verbosity":"low"},"tools":[{"type":"web_search","filters":{"allowed_domains":["b.com","a.com"]}}]}`)
	payload := map[string]any{
		"type":      "response.create",
		"model":     "m",
		"stream":    true,
		"input":     []json.RawMessage{json.RawMessage(`{"type":"x"}`)},
		"reasoning": map[string]any{"z": 1, "a": 2},
		"text":      map[string]any{"format": map[string]any{"type": "text"}, "verbosity": "low"},
		"tools":     []any{map[string]any{"type": "web_search"}},
	}
	out, err := marshalCodexWSCreateFrame(body, payload, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	// reasoning 不在官方序列的键按字典序 → "a" 在 "z" 前（struct 字段不存在透传）。
	if !strings.Contains(s, `"reasoning":{"a":2,"z":1}`) {
		t.Fatalf("reasoning serde order expected: %s", s)
	}
	// text 按 verbosity,format 序；format 内层按 type,strict,schema,name。
	if !strings.Contains(s, `"text":{"verbosity":"low","format":{"type":"text"}}`) {
		t.Fatalf("text serde order expected: %s", s)
	}
	// tools 保留客户端原始字节（含 filters 内部顺序）。
	if !strings.Contains(s, `"allowed_domains":["b.com","a.com"]`) {
		t.Fatalf("tools raw passthrough lost: %s", s)
	}
}

func TestMarshalCodexWSCreateFrame_DeltaShape(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"a"},{"type":"b"},{"type":"c"}]}`)
	payload := map[string]any{
		"type":                 "response.create",
		"model":                "m",
		"stream":               true,
		"previous_response_id": "resp_9",
		"input":                []json.RawMessage{json.RawMessage(`{"type":"c"}`)},
	}
	out, err := marshalCodexWSCreateFrame(body, payload, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	posPrev := strings.Index(s, `"previous_response_id"`)
	posInput := strings.Index(s, `"input"`)
	if posPrev < 0 || posPrev > posInput {
		t.Fatalf("previous_response_id must precede input: %s", s)
	}
	if !strings.Contains(s, `"input":[{"type":"c"}]`) {
		t.Fatalf("delta input expected: %s", s)
	}
}

func TestMarshalCodexWSCreateFrame_Prewarm(t *testing.T) {
	payload := map[string]any{
		"type":     "response.create",
		"model":    "m",
		"stream":   true,
		"input":    []json.RawMessage{},
		"generate": false,
	}
	out, err := marshalCodexWSCreateFrame(nil, payload, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"generate":false`) {
		t.Fatalf("generate=false missing: %s", s)
	}
	// generate 在 serde 序里位于 text 之后、client_metadata 之前（即 input 之后）。
	posGen := strings.Index(s, `"generate"`)
	posInput := strings.Index(s, `"input"`)
	if posGen < posInput {
		t.Fatalf("generate must follow input: %s", s)
	}
}

func TestMarshalCodexWSCreateFrame_OfficialDefaults(t *testing.T) {
	// 官方 ResponseCreateWsRequest 的恒发字段：stream 恒 true、tool_choice 恒
	// "auto"、store 恒 false、input/include/parallel_tool_calls/reasoning 恒
	// 序列化——客户端缺省时由序列化层补齐（client.rs build_responses_request）。
	payload := map[string]any{
		"type":  "response.create",
		"model": "m",
	}
	out, err := marshalCodexWSCreateFrame(nil, payload, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	for _, frag := range []string{
		`"stream":true`, `"input":[]`, `"tool_choice":"auto"`,
		`"parallel_tool_calls":false`, `"reasoning":{}`, `"store":false`, `"include":[]`,
	} {
		if !strings.Contains(s, frag) {
			t.Fatalf("official default missing %q in %s", frag, s)
		}
	}
	// 缺省字段按 serde 序插入各自位置而非堆到尾部队列。
	if strings.Index(s, `"tool_choice"`) <= strings.Index(s, `"input"`) ||
		strings.Index(s, `"tool_choice"`) >= strings.Index(s, `"parallel_tool_calls"`) ||
		strings.Index(s, `"reasoning"`) >= strings.Index(s, `"store"`) ||
		strings.Index(s, `"store"`) >= strings.Index(s, `"include"`) {
		t.Fatalf("defaults must land at serde positions: %s", s)
	}
	// stream 官方恒 true：客户端显式 false 也按 true 发（该端点总是回事件流）。
	payload["stream"] = false
	out, err = marshalCodexWSCreateFrame(nil, payload, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Fatalf("stream must be forced true: %s", out)
	}
	// 客户端显式值不被覆盖。
	payload["tool_choice"] = "required"
	out, err = marshalCodexWSCreateFrame(nil, payload, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"tool_choice":"required"`) {
		t.Fatalf("explicit tool_choice must be preserved: %s", out)
	}
	// 不污染调用方 payload。
	if _, ok := payload["tool_choice"]; !ok || payload["tool_choice"] != "required" {
		t.Fatalf("caller payload mutated: %v", payload)
	}
	if _, ok := payload["include"]; ok {
		t.Fatalf("caller payload got injected include")
	}
}

func TestCodexWSInstructionsBecomesDeveloperItem(t *testing.T) {
	// 官方 WS 帧无 instructions 字段：buildOpenAIWSCreatePayload 把它下沉为
	// input 首位 developer message（形态对齐基线 model.base_instructions）。
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":       "tok",
			"chatgpt_account_id": "acc",
		},
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	payload := svc.buildOpenAIWSCreatePayload(map[string]any{
		"model":        "m",
		"instructions": "SYS PROMPT",
		"input":        []any{map[string]any{"type": "message", "role": "user"}},
	}, account)
	if _, ok := payload["instructions"]; ok {
		t.Fatalf("top-level instructions must be removed for codex WS lane: %v", payload)
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("input must be [devItem, original]: %v", payload["input"])
	}
	rawItem, _ := input[0].(json.RawMessage)
	if len(rawItem) == 0 {
		t.Fatalf("instructions item must be ordered raw message: %v", input[0])
	}
	// serde 声明序：type,id,role,content,internal_chat_message_metadata_passthrough。
	if !strings.HasPrefix(string(rawItem), `{"type":"message","id":"msg_`) ||
		!strings.Contains(string(rawItem), `","role":"developer","content":[{"type":"input_text","text":"SYS PROMPT"}]`) {
		t.Fatalf("instructions item must be developer message in serde order: %s", rawItem)
	}
	var item map[string]any
	if err := json.Unmarshal(rawItem, &item); err != nil {
		t.Fatalf("item unmarshal: %v", err)
	}
	id, _ := item["id"].(string)
	meta, _ := item["internal_chat_message_metadata_passthrough"].(map[string]any)
	kinds, _ := meta["content_item_kinds"].([]any)
	if len(kinds) != 1 || kinds[0] != "model.base_instructions" {
		t.Fatalf("metadata passthrough must mark model.base_instructions: %v", meta)
	}
	// id 确定性：同一 instructions 两次注入得同一逻辑 item（delta 前缀命中）。
	payload2 := svc.buildOpenAIWSCreatePayload(map[string]any{
		"model": "m", "instructions": "SYS PROMPT",
	}, account)
	input2, _ := payload2["input"].([]any)
	if len(input2) == 0 {
		t.Fatalf("second payload input must contain dev item")
	}
	rawItem2, _ := input2[0].(json.RawMessage)
	if !strings.Contains(string(rawItem2), `"id":"`+id+`"`) {
		t.Fatalf("item id must be deterministic per content: %s vs %s", rawItem2, id)
	}
	// 端到端：marshal 后帧内不得出现顶层 instructions。
	out, err := marshalCodexWSCreateFrame(nil, payload, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), `"instructions"`) {
		t.Fatalf("frame must not carry top-level instructions: %s", out)
	}
	// 非 codex 账号不受影响。
	plain := svc.buildOpenAIWSCreatePayload(map[string]any{"instructions": "SYS"}, nil)
	if plain["instructions"] != "SYS" {
		t.Fatalf("non-codex lane must keep top-level instructions: %v", plain)
	}
}

func TestMarshalCodexWSCreateFrame_NonASCIIEscape(t *testing.T) {
	payload := map[string]any{
		"type":  "response.create",
		"model": "m",
		"input": []json.RawMessage{json.RawMessage(`{"type":"message","content":[{"type":"input_text","text":"\u4f60\u597d"}]}`)},
	}
	out, err := marshalCodexWSCreateFrame(nil, payload, false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// to_ascii_json_string: non-ASCII escapes to lowercase \uXXXX (U+4F60 U+597D).
	if !strings.Contains(string(out), `\u4f60\u597d`) {
		t.Fatalf("non-ascii must be escaped per to_ascii_json_string: %s", out)
	}
}
