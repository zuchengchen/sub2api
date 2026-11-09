package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func assistantDiagnosticFixture() (*userCacheDiagnostic, *userCacheAssistantHasher) {
	return &userCacheDiagnostic{config: &userCacheDiagnosticConfig{userID: 358}, apiKeyID: 644}, &userCacheAssistantHasher{entropy: bytes.NewReader(bytes.Repeat([]byte{7}, 32))}
}

func TestUserCacheDiagnosticAssistantCanonicalEquivalence(t *testing.T) {
	d, h := assistantDiagnosticFixture()
	base := d.summarizeAssistantWithHasher([]byte(`{"role":"assistant","content":"safe","future":{"b":2,"a":9007199254740993}}`), h)
	reordered := d.summarizeAssistantWithHasher([]byte(`{ "future": {"a":9007199254740993,"b":2}, "content":"safe", "role":"assistant" }`), h)
	require.True(t, base.Available)
	require.Equal(t, base.Object.HMAC, reordered.Object.HMAC)
	require.Equal(t, base.Native.HMAC, reordered.Native.HMAC)
	require.Equal(t, base.Unknown.HMAC, reordered.Unknown.HMAC)
	empty := d.summarizeAssistantWithHasher([]byte(`{"role":"assistant"}`), h)
	for _, raw := range []string{
		`{"role":"assistant","content":null,"reasoning_content":"","refusal":null,"name":"","tool_calls":[]}`,
		`{"role":"assistant","content":"","reasoning_content":null,"refusal":"","name":null,"tool_calls":null}`,
	} {
		got := d.summarizeAssistantWithHasher([]byte(raw), h)
		require.True(t, got.Native.Valid)
		require.NotEqual(t, empty.Object.HMAC, got.Object.HMAC)
		require.Equal(t, empty.Native.HMAC, got.Native.HMAC)
	}
	alias := d.summarizeAssistantWithHasher([]byte(`{"role":"assistant","reasoning":""}`), h)
	require.NotEqual(t, empty.Native.HMAC, alias.Native.HMAC, "actual native normalizer does not remove the reasoning alias")
}

func TestUserCacheDiagnosticAssistantSignificantFieldsAndRedaction(t *testing.T) {
	d, h := assistantDiagnosticFixture()
	base := d.summarizeAssistantWithHasher([]byte(`{"role":"assistant","content":"BODY_PRIVATE","reasoning_content":"THOUGHT_PRIVATE","tool_calls":[{"id":"CALL_PRIVATE","type":"function","function":{"name":"NAME_PRIVATE","arguments":"{\"value\":1}"}}],"UNKNOWN_PRIVATE":{"TOKEN_PRIVATE":"VALUE_PRIVATE"}}`), h)
	require.True(t, base.Available)
	require.True(t, base.Native.Valid)
	require.Equal(t, 1, base.Unknown.Count)
	require.Equal(t, len(userCacheAssistantFields), len(base.Fields))
	encoded, err := json.Marshal(base)
	require.NoError(t, err)
	for _, secret := range []string{"BODY_PRIVATE", "THOUGHT_PRIVATE", "CALL_PRIVATE", "NAME_PRIVATE", "UNKNOWN_PRIVATE", "TOKEN_PRIVATE", "VALUE_PRIVATE"} {
		require.NotContains(t, string(encoded), secret)
	}
	for _, tc := range []struct{ name, first, second, field string }{
		{"content", `{"role":"assistant","content":"a"}`, `{"role":"assistant","content":"b"}`, "content"},
		{"reasoning_content", `{"role":"assistant","reasoning_content":"a"}`, `{"role":"assistant"}`, "reasoning_content"},
		{"reasoning_alias", `{"role":"assistant","reasoning_content":"a"}`, `{"role":"assistant","reasoning":"a"}`, "reasoning_content"},
		{"refusal", `{"role":"assistant","refusal":"a"}`, `{"role":"assistant","refusal":"b"}`, "refusal"},
		{"name", `{"role":"assistant","name":"a"}`, `{"role":"assistant","name":"b"}`, "name"},
		{"tool_order", `{"role":"assistant","tool_calls":[{"id":"a"},{"id":"b"}]}`, `{"role":"assistant","tool_calls":[{"id":"b"},{"id":"a"}]}`, "tool_calls"},
		{"argument_string_whitespace", `{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"x\":1}"}}]}`, `{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"x\": 1}"}}]}`, "tool_calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := d.summarizeAssistantWithHasher([]byte(tc.first), h)
			b := d.summarizeAssistantWithHasher([]byte(tc.second), h)
			require.True(t, a.Available && b.Available)
			require.NotEqual(t, a.Native.HMAC, b.Native.HMAC)
			require.NotEqual(t, a.Fields[tc.field].HMAC, b.Fields[tc.field].HMAC)
		})
	}
	u1 := d.summarizeAssistantWithHasher([]byte(`{"role":"assistant","DYNAMIC_ONE":1}`), h)
	u2 := d.summarizeAssistantWithHasher([]byte(`{"role":"assistant","DYNAMIC_TWO":1}`), h)
	require.NotEqual(t, u1.Unknown.HMAC, u2.Unknown.HMAC)
	require.NotEqual(t, u1.Native.HMAC, u2.Native.HMAC)
	legacy := d.summarizeAssistantWithHasher([]byte(`{"role":"assistant","function_call":{"name":"x"}}`), h)
	require.True(t, legacy.Available)
	require.False(t, legacy.Native.Valid)
	require.Empty(t, legacy.Native.HMAC)
}

func TestUserCacheDiagnosticAssistantTailBinding(t *testing.T) {
	d, h := assistantDiagnosticFixture()
	var messages []json.RawMessage
	for i := 0; i < 6; i++ {
		messages = append(messages, json.RawMessage(`{"role":"user","content":"question"}`), json.RawMessage(fmt.Sprintf(`{"role":"assistant","content":"answer-%d"}`, i)))
	}
	body, err := json.Marshal(map[string]any{"messages": messages})
	require.NoError(t, err)
	before := bytes.Clone(body)
	got := d.summarizeAssistantTailWithHasher(body, h)
	require.Equal(t, before, body)
	require.True(t, got.Available)
	require.Equal(t, 6, got.AssistantCount)
	require.Equal(t, 2, got.OmittedAssistants)
	require.Len(t, got.Items, 4)
	for i, item := range got.Items {
		index := 5 + 2*i
		require.Equal(t, index, item.Index)
		require.Equal(t, summarizeUserCacheValue(messages[index]), item.Value)
		delivery := d.summarizeAssistantWithHasher(messages[index], h)
		require.Equal(t, delivery.Native.HMAC, item.Comparison.Native.HMAC)
		require.Equal(t, delivery.Fields, item.Comparison.Fields)
	}
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), userCacheAssistantMaxSummary)
}

func TestUserCacheDiagnosticAssistantRejectsAmbiguityAndLimits(t *testing.T) {
	d, h := assistantDiagnosticFixture()
	for _, tc := range []struct{ name, raw, reason string }{
		{"duplicate_root", `{"role":"assistant","role":"assistant"}`, "invalid_json"},
		{"nested_duplicate", `{"role":"assistant","unknown":{"key":1,"key":2}}`, "invalid_json"},
		{"multiple_roots", `{"role":"assistant"} {}`, "invalid_json"},
		{"nonassistant", `{"role":"user"}`, "not_assistant"},
		{"invalid_utf8", `{"role":"assistant","content":"` + string([]byte{0xff}) + `"}`, "invalid_json"},
		{"message_limit", `{"role":"assistant","content":"` + strings.Repeat("a", userCacheAssistantMaxMessage) + `"}`, "message_limit"},
		{"canonical_expansion_limit", `{"role":"assistant","content":"` + strings.Repeat("<", userCacheAssistantMaxMessage/2) + `"}`, "canonical_limit"},
		{"depth", `{"role":"assistant","x":` + strings.Repeat("[", 66) + `0` + strings.Repeat("]", 66) + `}`, "invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := d.summarizeAssistantWithHasher([]byte(tc.raw), h)
			require.False(t, got.Available)
			require.Equal(t, tc.reason, got.Reason)
			require.Nil(t, got.Fields)
			require.Empty(t, got.Native.HMAC)
		})
	}
	for _, raw := range []string{
		`{"messages":[],"messages":[{"role":"assistant"}]}`,
		`{"messages":[{"role":"user","unknown":{"x":1,"x":2}},{"role":"assistant"}]}`,
		`{"messages":null}`, `{"messages":{}}`, `{"other":1}`,
		`{"messages":[{"role":"assistant"}],"Messages":[]}`,
		`{"messages":[{"role":"assistant","content":"` + string([]byte{0xff}) + `"}]}`,
	} {
		got := d.summarizeAssistantTailWithHasher([]byte(raw), h)
		require.False(t, got.Available)
		require.Nil(t, got.Items)
	}
	tooMany := `{"messages":[` + strings.Repeat(`{"role":"assistant"},`, userCacheDiagnosticMaxItems) + `{"role":"assistant"}]}`
	require.Equal(t, "history_summary_truncated", d.summarizeAssistantTailWithHasher([]byte(tooMany), h).Reason)
	require.Equal(t, "body_limit", d.summarizeAssistantTailWithHasher(bytes.Repeat([]byte(" "), userCacheAssistantMaxBody+1), h).Reason)
	// Four maximum-size raw messages stay under the aggregate input limit;
	// canonical expansion and actual output encoding are independently bounded.
	large := `{"role":"assistant","content":"` + strings.Repeat("a", userCacheAssistantMaxMessage-64) + `"}`
	body := `{"messages":[` + strings.Join([]string{large, large, large, large}, ",") + `]}`
	got := d.summarizeAssistantTailWithHasher([]byte(body), h)
	require.True(t, got.Available)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), userCacheAssistantMaxSummary)
}

type assistantDiagnosticFailReader struct{ calls int }

func (r *assistantDiagnosticFailReader) Read([]byte) (int, error) {
	r.calls++
	return 0, errors.New("synthetic entropy failure")
}

func TestUserCacheDiagnosticAssistantHMACScopeAndEntropy(t *testing.T) {
	d, h := assistantDiagnosticFixture()
	raw := []byte(`{"role":"assistant","content":"same","refusal":"same"}`)
	base := d.summarizeAssistantWithHasher(raw, h)
	otherUser := &userCacheDiagnostic{config: &userCacheDiagnosticConfig{userID: 359}, apiKeyID: 644}
	otherKey := &userCacheDiagnostic{config: &userCacheDiagnosticConfig{userID: 358}, apiKeyID: 645}
	otherProcess := &userCacheAssistantHasher{entropy: bytes.NewReader(bytes.Repeat([]byte{8}, 32))}
	require.NotEqual(t, base.Native.HMAC, otherUser.summarizeAssistantWithHasher(raw, h).Native.HMAC)
	require.NotEqual(t, base.Native.HMAC, otherKey.summarizeAssistantWithHasher(raw, h).Native.HMAC)
	require.NotEqual(t, base.Native.HMAC, d.summarizeAssistantWithHasher(raw, otherProcess).Native.HMAC)
	require.NotEqual(t, base.Object.HMAC, base.Native.HMAC, "digest purpose is framed even when native and object canonical bytes match")
	require.NotEqual(t, base.Fields["content"].HMAC, base.Fields["refusal"].HMAC)
	require.False(t, base.Fields["name"].Present)
	require.Equal(t, "missing", base.Fields["name"].Type)
	require.Empty(t, base.Fields["name"].HMAC)
	failure := &assistantDiagnosticFailReader{}
	bad := &userCacheAssistantHasher{entropy: failure}
	for i := 0; i < 2; i++ {
		got := d.summarizeAssistantWithHasher(raw, bad)
		require.False(t, got.Available)
		require.Equal(t, "entropy_unavailable", got.Reason)
	}
	require.Equal(t, 1, failure.calls)
	require.Equal(t, [32]byte{}, bad.key)
	var absent *userCacheDiagnostic
	require.Equal(t, "invalid_scope", absent.summarizeAssistant(raw).Reason)
	require.Equal(t, "invalid_scope", (&userCacheDiagnostic{}).summarizeAssistantTail([]byte(`{}`)).Reason)
}

func TestUserCacheDiagnosticAssistantConcurrent(t *testing.T) {
	d, h := assistantDiagnosticFixture()
	raw := []byte(`{"role":"assistant","content":"same"}`)
	var wg sync.WaitGroup
	results := make(chan string, 64)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- d.summarizeAssistantWithHasher(raw, h).Native.HMAC }()
	}
	wg.Wait()
	close(results)
	var first string
	for got := range results {
		require.Len(t, got, 64)
		if first == "" {
			first = got
		}
		require.Equal(t, first, got)
	}
}
