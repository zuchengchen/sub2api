package service

import (
	"encoding/json"
	"testing"
	"time"
)

func deltaTestItems(t *testing.T, bodies ...string) []json.RawMessage {
	t.Helper()
	items := make([]json.RawMessage, 0, len(bodies))
	for _, b := range bodies {
		items = append(items, json.RawMessage(b))
	}
	return items
}

func TestResolveOpenAIWSDelta_PrefixExtension(t *testing.T) {
	base := deltaTestItems(t,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
		`{"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"}`,
	)
	chain := &openAIWSDeltaChainState{
		ResponseID:     "resp_1",
		AccountID:      7,
		FieldsFP:       "fp",
		BaselineHashes: openAIWSDeltaItemsHash(base),
	}
	current := append(append([]json.RawMessage{}, base...),
		json.RawMessage(`{"type":"function_call_output","call_id":"c1","output":"ok"}`))

	decision := resolveOpenAIWSDelta(chain, 7, "fp", current)
	if !decision.Delta {
		t.Fatalf("expected delta decision, got %s", decision.Reason)
	}
	if decision.PreviousResponse != "resp_1" {
		t.Fatalf("previous_response_id = %q", decision.PreviousResponse)
	}
	if len(decision.TailItems) != 1 {
		t.Fatalf("tail items = %d, want 1", len(decision.TailItems))
	}
}

func TestResolveOpenAIWSDelta_RejectsMutations(t *testing.T) {
	base := deltaTestItems(t, `{"type":"message","role":"user"}`)
	chain := &openAIWSDeltaChainState{
		ResponseID:     "resp_1",
		AccountID:      7,
		FieldsFP:       "fp",
		BaselineHashes: openAIWSDeltaItemsHash(base),
	}

	cases := []struct {
		name    string
		chain   *openAIWSDeltaChainState
		acct    int64
		fp      string
		current []json.RawMessage
		reason  string
	}{
		{"shortened input", chain, 7, "fp", deltaTestItems(t), "input_shortened"},
		{"changed prefix", chain, 7, "fp", deltaTestItems(t, `{"type":"message","role":"assistant"}`, `{"type":"x"}`), "input_mismatch"},
		{"fields changed", chain, 7, "other", append(append([]json.RawMessage{}, base...), json.RawMessage(`{"type":"x"}`)), "request_property_changed"},
		{"account mismatch", chain, 8, "fp", append(append([]json.RawMessage{}, base...), json.RawMessage(`{"type":"x"}`)), "account_mismatch"},
		{"no chain", nil, 7, "fp", deltaTestItems(t, `{"type":"x"}`), "no_chain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := resolveOpenAIWSDelta(tc.chain, tc.acct, tc.fp, tc.current)
			if decision.Delta {
				t.Fatalf("expected non-delta decision")
			}
			if decision.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", decision.Reason, tc.reason)
			}
		})
	}
}

func TestResolveOpenAIWSDelta_NormalizedItemCompare(t *testing.T) {
	// 客户端以不同键序/空白重发同一 item 时仍应命中前缀。
	stored := deltaTestItems(t, `{"type":"message","role":"user","content":[{"type":"input_text","text":"a"}]}`)
	resent := deltaTestItems(t, `{ "role":"user", "type":"message", "content":[ { "text":"a", "type":"input_text" } ] }`,
		`{"type":"function_call_output","call_id":"c","output":"x"}`)
	chain := &openAIWSDeltaChainState{
		ResponseID:     "resp_x",
		AccountID:      1,
		BaselineHashes: openAIWSDeltaItemsHash(stored),
	}
	decision := resolveOpenAIWSDelta(chain, 1, "", resent)
	if !decision.Delta {
		t.Fatalf("expected delta via normalized compare, got %s", decision.Reason)
	}
	if len(decision.TailItems) != 1 {
		t.Fatalf("tail = %d", len(decision.TailItems))
	}
}

func TestOpenAIWSDeltaRecorder(t *testing.T) {
	r := &openAIWSDeltaRecorder{}
	r.observe([]byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}`))
	r.observe([]byte(`{"type":"response.output_item.done","item":{"type":"reasoning","summary":[]}}`))
	r.observe([]byte(`{"type":"response.completed","response":{"id":"resp_42","status":"completed"}}`))

	logical := openAIWSDeltaItemsHash(deltaTestItems(t, `{"type":"message","role":"user"}`))
	next, ok := r.finalize(9, "fp", logical, nil)
	if !ok {
		t.Fatal("expected finalize to produce chain")
	}
	if next.ResponseID != "resp_42" {
		t.Fatalf("response id = %q", next.ResponseID)
	}
	if next.AccountID != 9 {
		t.Fatalf("account id = %d", next.AccountID)
	}
	if len(next.BaselineHashes) != 3 {
		t.Fatalf("baseline = %d, want 3 (1 logical + 2 items_added)", len(next.BaselineHashes))
	}
}

func TestOpenAIWSDeltaRecorder_NoCompleteNoChain(t *testing.T) {
	r := &openAIWSDeltaRecorder{}
	r.observe([]byte(`{"type":"response.output_item.done","item":{"type":"message"}}`))
	_, ok := r.finalize(1, "fp", nil, nil)
	if ok {
		t.Fatal("incomplete response must not extend chain")
	}
}

func TestOpenAIWSDeltaFieldsFingerprint(t *testing.T) {
	payload := map[string]any{
		"model":                "gpt-5",
		"stream":               true,
		"store":                false,
		"tools":                []any{map[string]any{"type": "function"}},
		"reasoning":            map[string]any{"effort": "low"},
		"input":                []any{map[string]any{"type": "message"}},
		"client_metadata":      map[string]any{"a": "b"},
		"previous_response_id": "resp_x",
		"stream_options":       map[string]any{"include_obfuscation": true},
	}
	fp1 := openAIWSDeltaFieldsFingerprint(payload)
	// 改动 input/metadata/stream_options/prev_id 不应影响指纹。
	payload["input"] = []any{map[string]any{"type": "other"}}
	payload["client_metadata"] = map[string]any{"a": "c"}
	payload["previous_response_id"] = "resp_y"
	payload["stream_options"] = nil
	if openAIWSDeltaFieldsFingerprint(payload) != fp1 {
		t.Fatal("fingerprint must ignore input/metadata/prev_id/stream_options")
	}
	payload["model"] = "gpt-6"
	if openAIWSDeltaFieldsFingerprint(payload) == fp1 {
		t.Fatal("model change must alter fingerprint")
	}
}

func TestDeltaChainStoreRoundtrip(t *testing.T) {
	store := NewOpenAIWSStateStore(nil)
	state := &openAIWSDeltaChainState{
		ResponseID:     "resp_z",
		AccountID:      3,
		FieldsFP:       "fp",
		BaselineHashes: []string{"h1", "h2"},
	}
	store.BindSessionDeltaChain(4, "sess", state, time.Minute)
	got, ok := store.GetSessionDeltaChain(4, "sess")
	if !ok {
		t.Fatal("expected chain hit")
	}
	if got.ResponseID != "resp_z" || got.AccountID != 3 || len(got.BaselineHashes) != 2 {
		t.Fatalf("unexpected chain %+v", got)
	}
	store.DeleteSessionDeltaChain(4, "sess")
	if _, ok := store.GetSessionDeltaChain(4, "sess"); ok {
		t.Fatal("expected chain deleted")
	}
}

func TestOpenAIWSResponseProducedOnConn(t *testing.T) {
	store := NewOpenAIWSStateStore(nil)
	store.BindResponseConn("resp_1", "conn_a", time.Hour)
	if _, same := openAIWSResponseProducedOnConn(store, "resp_1", "conn_a"); !same {
		t.Fatal("producing socket must continue its own response")
	}
	producer, same := openAIWSResponseProducedOnConn(store, "resp_1", "conn_b")
	if same || producer != "conn_a" {
		t.Fatalf("another socket cannot continue resp_1: producer=%q same=%v", producer, same)
	}
	if _, same := openAIWSResponseProducedOnConn(store, "resp_unknown", "conn_a"); same {
		t.Fatal("unknown producer must fall back to a full request")
	}
}
