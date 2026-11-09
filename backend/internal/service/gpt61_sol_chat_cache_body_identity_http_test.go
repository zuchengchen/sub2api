package service_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const gpt61BodyIdentityModel = "gpt-6.1-sol"

func gpt61BodyIdentityHarness(t *testing.T) *gpt61ScopeHTTPHarness {
	t.Helper()
	return newGPT61ScopeHTTPHarness(t, &gpt61ScopeHTTPPolicy{config: service.NativeReasoningReplayRuntimeConfig{Mode: "replay", AllUsersModels: []string{gpt61BodyIdentityModel}}}, nil, "")
}

// No session header, prompt-cache hint, or legacy user allowlist supplies these
// body identities. Only the actual HTTP request decorator changes the carrier.
func gpt61BodyCarrier(kind, session, thread string) func(map[string]any, http.Header) {
	return func(body map[string]any, _ http.Header) {
		meta := map[string]any{}
		if session != "" {
			meta["session_id"] = session
		}
		if thread != "" {
			meta["thread_id"] = thread
		}
		if kind == "embedded" {
			body["client_metadata"] = map[string]any{"x-codex-turn-metadata": string(nativeTrafficJSON(meta))}
		} else {
			body["client_metadata"] = meta
		}
	}
}
func gpt61BodyReasoningCount(raw []byte) int {
	count := 0
	gjson.GetBytes(raw, "input").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "reasoning" {
			count++
		}
		return true
	})
	return count
}
func gpt61BodyRequireRestored(t *testing.T, row nativeTrafficRow, old ...nativeTrafficRow) {
	t.Helper()
	require.Equal(t, len(old), gpt61BodyReasoningCount(row.Body))
	for _, v := range old {
		require.Zero(t, nativeTrafficComparisonExit(row.Body, v.Output), "all native fields, opaque state, and tool relationships must match exactly")
	}
}

func TestGPT61SolChatCacheBodyIdentityHTTPRestore(t *testing.T) {
	for _, carrier := range []string{"flat", "embedded", "thread_only"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", carrier, stream), func(t *testing.T) {
				h := gpt61BodyIdentityHarness(t)
				who := gpt61ScopeHTTPClient{user: 101, key: 501}
				session := "body-session"
				if carrier == "thread_only" {
					session = ""
				}
				decorate := gpt61BodyCarrier(carrier, session, "body-thread")
				history := nativeTrafficInitialHistory()
				previous := []nativeTrafficRow{}
				for round := 0; round < 3; round++ {
					assistant, row, err := h.callWithRequest(history, gpt61BodyIdentityModel, stream, who, decorate)
					require.NoError(t, err)
					gpt61BodyRequireRestored(t, row, previous...)
					previous = append(previous, row)
					history = nativeTrafficContinue(history, assistant)
				}
				h.assertNoForwardErrors(t)
				require.EqualValues(t, 1, h.policy.reads.Load())
			})
		}
	}
}

func TestGPT61SolChatCacheBodyIdentityHTTPThreadIsolation(t *testing.T) {
	for _, carrier := range []string{"flat", "embedded"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", carrier, stream), func(t *testing.T) {
				h := gpt61BodyIdentityHarness(t)
				who := gpt61ScopeHTTPClient{user: 101, key: 501}
				initial := nativeTrafficInitialHistory()
				first, firstRow, err := h.callWithRequest(initial, gpt61BodyIdentityModel, stream, who, gpt61BodyCarrier(carrier, "same-session", "thread-a"))
				require.NoError(t, err)
				continued := nativeTrafficContinue(initial, first)
				second, otherRow, err := h.callWithRequest(continued, gpt61BodyIdentityModel, stream, who, gpt61BodyCarrier(carrier, "same-session", "thread-b"))
				require.NoError(t, err)
				gpt61BodyRequireRestored(t, otherRow)
				_, otherFollow, err := h.callWithRequest(nativeTrafficContinue(continued, second), gpt61BodyIdentityModel, stream, who, gpt61BodyCarrier(carrier, "same-session", "thread-b"))
				require.NoError(t, err)
				gpt61BodyRequireRestored(t, otherFollow, otherRow)
				require.NotZero(t, nativeTrafficComparisonExit(otherFollow.Body, firstRow.Output))
				_, originalFollow, err := h.callWithRequest(continued, gpt61BodyIdentityModel, stream, who, gpt61BodyCarrier(carrier, "same-session", "thread-a"))
				require.NoError(t, err)
				gpt61BodyRequireRestored(t, originalFollow, firstRow)
				h.assertNoForwardErrors(t)
			})
		}
	}
}

func TestGPT61SolChatCacheBodyIdentityHTTPParentFailsClosed(t *testing.T) {
	for _, carrier := range []string{"flat", "embedded"} {
		for _, hasSession := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/session_%t/stream_%t", carrier, hasSession, stream), func(t *testing.T) {
					h := gpt61BodyIdentityHarness(t)
					who := gpt61ScopeHTTPClient{user: 101, key: 501}
					history := nativeTrafficInitialHistory()
					decorate := func(body map[string]any, _ http.Header) {
						meta := map[string]any{"parent_thread_id": "known-parent"}
						if hasSession {
							meta["session_id"] = "parent-session"
						}
						if carrier == "embedded" {
							body["client_metadata"] = map[string]any{"x-codex-turn-metadata": string(nativeTrafficJSON(meta))}
						} else {
							body["client_metadata"] = meta
						}
					}
					for round := 0; round < 3; round++ {
						assistant, row, err := h.callWithRequest(history, gpt61BodyIdentityModel, stream, who, decorate)
						require.NoError(t, err)
						gpt61BodyRequireRestored(t, row)
						history = nativeTrafficContinue(history, assistant)
					}
					h.store.mu.Lock()
					puts := h.store.puts
					h.store.mu.Unlock()
					require.Zero(t, puts, "a parent without a child/thread or turn must not publish replayable state")
					h.assertNoForwardErrors(t)
				})
			}
		}
	}
}

func TestGPT61SolChatCacheBodyIdentityHTTPPrecedence(t *testing.T) {
	for _, carrier := range []string{"flat", "embedded"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", carrier, stream), func(t *testing.T) {
				h := gpt61BodyIdentityHarness(t)
				who := gpt61ScopeHTTPClient{user: 101, key: 501}
				history := nativeTrafficInitialHistory()
				previous := []nativeTrafficRow{}
				for round := 0; round < 3; round++ {
					decorate := func(body map[string]any, headers http.Header) {
						gpt61BodyCarrier(carrier, "winning-body-session", "winning-body-thread")(body, headers)
						if carrier == "embedded" {
							cm := body["client_metadata"].(map[string]any)
							cm["session_id"] = fmt.Sprintf("losing-flat-session-%d", round)
							cm["thread_id"] = fmt.Sprintf("losing-flat-thread-%d", round)
						}
						headers.Set("session-id", fmt.Sprintf("losing-header-session-%d", round))
						headers.Set("thread-id", fmt.Sprintf("losing-header-thread-%d", round))
						headers.Set("X-Session-Affinity", fmt.Sprintf("losing-alias-%d", round))
						headers.Set("x-codex-turn-metadata", string(nativeTrafficJSON(map[string]any{"session_id": fmt.Sprintf("losing-json-session-%d", round), "thread_id": fmt.Sprintf("losing-json-thread-%d", round)})))
					}
					assistant, row, err := h.callWithRequest(history, gpt61BodyIdentityModel, stream, who, decorate)
					require.NoError(t, err)
					gpt61BodyRequireRestored(t, row, previous...)
					previous = append(previous, row)
					history = nativeTrafficContinue(history, assistant)
				}
				// Reverse the conflict: retaining the old identity only in headers must
				// not resurrect its state after the body explicitly selects another one.
				_, changed, err := h.callWithRequest(history, gpt61BodyIdentityModel, stream, who, func(body map[string]any, headers http.Header) {
					gpt61BodyCarrier(carrier, "different-body-session", "different-body-thread")(body, headers)
					headers.Set("session-id", "winning-body-session")
					headers.Set("thread-id", "winning-body-thread")
				})
				require.NoError(t, err)
				gpt61BodyRequireRestored(t, changed)
				h.assertNoForwardErrors(t)
			})
		}
	}
}

func TestGPT61SolChatCacheBodyIdentityHTTPUserKeyIsolation(t *testing.T) {
	for _, carrier := range []string{"flat", "embedded", "thread_only"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", carrier, stream), func(t *testing.T) {
				h := gpt61BodyIdentityHarness(t)
				owner := gpt61ScopeHTTPClient{user: 101, key: 501}
				session := "shared-body-session"
				if carrier == "thread_only" {
					session = ""
				}
				decorate := gpt61BodyCarrier(carrier, session, "shared-body-thread")
				initial := nativeTrafficInitialHistory()
				first, firstRow, err := h.callWithRequest(initial, gpt61BodyIdentityModel, stream, owner, decorate)
				require.NoError(t, err)
				continued := nativeTrafficContinue(initial, first)
				for _, other := range []gpt61ScopeHTTPClient{{user: 102, key: 501}, {user: 101, key: 502}} {
					assistant, otherRow, err := h.callWithRequest(continued, gpt61BodyIdentityModel, stream, other, decorate)
					require.NoError(t, err)
					gpt61BodyRequireRestored(t, otherRow)
					_, follow, err := h.callWithRequest(nativeTrafficContinue(continued, assistant), gpt61BodyIdentityModel, stream, other, decorate)
					require.NoError(t, err)
					gpt61BodyRequireRestored(t, follow, otherRow)
					require.NotZero(t, nativeTrafficComparisonExit(follow.Body, firstRow.Output))
				}
				_, ownerFollow, err := h.callWithRequest(continued, gpt61BodyIdentityModel, stream, owner, decorate)
				require.NoError(t, err)
				gpt61BodyRequireRestored(t, ownerFollow, firstRow)
				h.assertNoForwardErrors(t)
			})
		}
	}
}
