package service

import (
	"encoding/json"
	"strings"
	"testing"
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
	out, err := marshalCodexWSCreateFrame(body, payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	// serde 序：type,model,stream,...,previous_response_id(缺),input,tools,tool_choice,
	// parallel_tool_calls,reasoning,store,stream_options,include,prompt_cache_key,
	// text,generate,client_metadata — instructions 不是官方字段应排在末尾未知键区。
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
	out, err := marshalCodexWSCreateFrame(body, payload)
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
	out, err := marshalCodexWSCreateFrame(body, payload)
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
	out, err := marshalCodexWSCreateFrame(nil, payload)
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

func TestMarshalCodexWSCreateFrame_NonASCIIEscape(t *testing.T) {
	payload := map[string]any{
		"type":  "response.create",
		"model": "m",
		"input": []json.RawMessage{json.RawMessage(`{"type":"message","content":[{"type":"input_text","text":"\u4f60\u597d"}]}`)},
	}
	out, err := marshalCodexWSCreateFrame(nil, payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// to_ascii_json_string: non-ASCII escapes to lowercase \uXXXX (U+4F60 U+597D).
	if !strings.Contains(string(out), `\u4f60\u597d`) {
		t.Fatalf("non-ascii must be escaped per to_ascii_json_string: %s", out)
	}
}
