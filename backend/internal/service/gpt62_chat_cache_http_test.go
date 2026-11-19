package service_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func gpt62ChatCachePolicy() *gpt61ScopeHTTPPolicy {
	return &gpt61ScopeHTTPPolicy{config: service.NativeReasoningReplayRuntimeConfig{
		Mode: service.NativeReasoningReplayModeReplay,
		ModelUserIDs: map[string][]int64{
			"gpt-6.2-astra": {101, 102},
			"gpt-6.2-sol":   {101},
		},
	}}
}

func TestGPT62ChatCacheHTTPModelUsers(t *testing.T) {
	longPrefix := strings.Repeat("Reference data. ", 1<<16) // 1 MiB of fixed system context.
	for _, tc := range []struct {
		name, model string
		user        int64
		stream      bool
		replay      bool
		prefix      string
	}{
		{"astra_stream", "gpt-6.2-astra", 101, true, true, ""},
		{"astra_buffered_long_context", "gpt-6.2-astra", 102, false, true, longPrefix},
		{"sol_stream", "gpt-6.2-sol", 101, true, true, ""},
		{"sol_buffered", "gpt-6.2-sol", 101, false, true, ""},
		{"different_model_list", "gpt-6.2-sol", 102, true, false, ""},
		{"unlisted_user", "gpt-6.2-astra", 103, false, false, ""},
		{"unlisted_model", "gpt-6.1-sol", 101, true, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newGPT61ScopeHTTPHarness(t, gpt62ChatCachePolicy(), nil, "")
			who := gpt61ScopeHTTPClient{user: tc.user, key: 501}
			var decorate func(map[string]any, http.Header)
			if tc.stream {
				who.session = "chat-session"
			} else {
				decorate = gpt61BodyCarrier("flat", "chat-session", "chat-thread")
			}
			history := nativeTrafficInitialHistory()
			if tc.prefix != "" {
				instructions := gjson.GetBytes(history[0], "content").String()
				history[0] = nativeTrafficJSON(map[string]string{"role": "system", "content": instructions + "\n" + tc.prefix})
			}
			var previous []nativeTrafficRow
			for round := range 3 {
				assistant, row, err := h.callWithRequest(history, tc.model, tc.stream, who, decorate)
				require.NoError(t, err)
				require.Equal(t, tc.model, gjson.GetBytes(row.Body, "model").String())
				if tc.prefix != "" {
					require.True(t, strings.Contains(string(row.Body), strings.TrimSpace(tc.prefix)), "the long system prefix must reach the upstream")
				}
				gpt61BodyRequireRestored(t, row, previous...)
				calls := gjson.GetBytes(row.Body, `input.#(type=="function_call")#.call_id`)
				results := gjson.GetBytes(row.Body, `input.#(type=="function_call_output")#.call_id`)
				require.Len(t, calls.Array(), 2*round)
				require.Equal(t, calls.Value(), results.Value(), "tool results must retain their matching call IDs")
				if tc.replay {
					previous = append(previous, row)
				}
				history = nativeTrafficContinue(history, assistant)
			}
			// The fixture also checks the gateway result against the 101 input,
			// 31 cached and 17 output tokens reported in each upstream response.
			h.assertNoForwardErrors(t)
			h.store.mu.Lock()
			puts := h.store.puts
			h.store.mu.Unlock()
			require.Equal(t, tc.replay, puts > 0)
		})
	}
}

func TestGPT62ChatCacheHTTPReplayIsolation(t *testing.T) {
	testChatCacheHTTPReplayIsolation(t, gpt62ChatCachePolicy(), "gpt-6.2-astra", "gpt-6.2-sol")
}

func TestGPTFamilyChatCacheHTTPReplayIsolation(t *testing.T) {
	policy := &gpt61ScopeHTTPPolicy{config: service.NativeReasoningReplayRuntimeConfig{Mode: "replay", AllUsersGPTModels: true}}
	testChatCacheHTTPReplayIsolation(t, policy, "gpt-5.6-sol", "gpt-6-astra")
}

func testChatCacheHTTPReplayIsolation(t *testing.T, policy *gpt61ScopeHTTPPolicy, model, otherModel string) {
	t.Helper()
	h := newGPT61ScopeHTTPHarness(t, policy, nil, "")
	owner := gpt61ScopeHTTPClient{user: 101, key: 501}
	firstThread := gpt61BodyCarrier("flat", "shared-session", "thread-a")
	initial := nativeTrafficInitialHistory()
	first, firstRow, err := h.callWithRequest(initial, model, true, owner, firstThread)
	require.NoError(t, err)
	continued := nativeTrafficContinue(initial, first)
	edited := append([]json.RawMessage(nil), continued...)
	edited[1] = nativeTrafficJSON(map[string]string{"role": "user", "content": "A different first question."})

	otherKey := owner
	otherKey.key++
	otherUser := owner
	otherUser.user = 102
	otherCredential := owner
	otherCredential.account = "rotated_credentials"
	for _, tc := range []struct {
		name, model, thread string
		who                 gpt61ScopeHTTPClient
		history             []json.RawMessage
	}{
		{"other_authorized_model", otherModel, "thread-a", owner, continued},
		{"other_thread", model, "thread-b", owner, continued},
		{"other_authorized_user", model, "thread-a", otherUser, continued},
		{"other_key", model, "thread-a", otherKey, continued},
		{"other_upstream_credential", model, "thread-a", otherCredential, continued},
		{"edited_history", model, "thread-a", owner, edited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, row, err := h.callWithRequest(tc.history, tc.model, true, tc.who, gpt61BodyCarrier("flat", "shared-session", tc.thread))
			require.NoError(t, err)
			gpt61BodyRequireRestored(t, row)
		})
	}
	_, original, err := h.callWithRequest(continued, model, true, owner, firstThread)
	require.NoError(t, err)
	gpt61BodyRequireRestored(t, original, firstRow)
	for _, path := range []string{"client_metadata.session_id", "client_metadata.thread_id"} {
		firstID := gjson.GetBytes(firstRow.Body, path).String()
		require.NotEmpty(t, firstID, path)
		require.Equal(t, firstID, gjson.GetBytes(original.Body, path).String(), path)
	}
	h.assertNoForwardErrors(t)
}

func TestGPTFamilyChatCacheHTTPAnonymousHistory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decorate func(map[string]any, http.Header)
	}{
		{"messages_only", nil},
		{"prompt_cache_key_only", func(body map[string]any, _ http.Header) { body["prompt_cache_key"] = "shared-cache-hint" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := &gpt61ScopeHTTPPolicy{config: service.NativeReasoningReplayRuntimeConfig{Mode: "replay", AllUsersGPTModels: true}}
			h := newGPT61ScopeHTTPHarness(t, policy, nil, "")
			history := nativeTrafficInitialHistory()
			who := gpt61ScopeHTTPClient{user: 103, key: 503}
			for range 2 {
				assistant, row, err := h.callWithRequest(history, "gpt-5.6-sol", false, who, tc.decorate)
				require.NoError(t, err)
				gpt61BodyRequireRestored(t, row)
				history = nativeTrafficContinue(history, assistant)
			}
			h.assertNoForwardErrors(t)
			h.store.mu.Lock()
			defer h.store.mu.Unlock()
			require.Zero(t, h.store.gets)
			require.Zero(t, h.store.puts)
		})
	}
}
