package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func requireUUIDv7(t *testing.T, raw string) {
	t.Helper()
	parsed, err := uuid.Parse(raw)
	require.NoError(t, err, raw)
	require.EqualValues(t, 7, parsed.Version(), raw)
}

func newTestTurnClock() *codexTurnClock {
	return &codexTurnClock{ttl: time.Hour, now: time.Now}
}

func TestCodexTurnIdentity_StableAcrossToolLoop(t *testing.T) {
	clock := newTestTurnClock()
	first := gjson.Parse(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}]}`)
	follow := gjson.Parse(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]},
		{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)
	id1, at1 := resolveCodexTurnIdentity(clock, "seed", "thread", collectCodexTurnEvidence(nil, nil, first))
	time.Sleep(3 * time.Millisecond)
	id2, at2 := resolveCodexTurnIdentity(clock, "seed", "thread", collectCodexTurnEvidence(nil, nil, follow))
	require.Equal(t, id1, id2, "tool-loop follow-up must stay in the same turn")
	require.Equal(t, at1, at2)
	requireUUIDv7(t, id1)
	require.Equal(t, at1, codexUUIDv7Millis(id1), "turn_id timestamp must equal turn_started_at")

	next := gjson.Parse(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}]}`)
	id3, _ := resolveCodexTurnIdentity(clock, "seed", "thread", collectCodexTurnEvidence(nil, nil, next))
	require.NotEqual(t, id1, id3, "a new user message (even with identical text) starts a new turn")
}

func TestCodexTurnIdentity_MapAndRawAgree(t *testing.T) {
	raw := `{"prompt_cache_key":"k","input":[{"role":"user","content":[{"text":"hi","type":"input_text"}]}]}`
	decoded := map[string]any{
		"prompt_cache_key": "k",
		"input":            []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
	}
	a := collectCodexTurnEvidence(nil, nil, gjson.Parse(raw))
	b := collectCodexTurnEvidenceFromMap(decoded, nil)
	require.NotEmpty(t, a.InputTurnSeed)
	require.Equal(t, a.InputTurnSeed, b.InputTurnSeed)
	require.Equal(t, a.PromptCacheKey, b.PromptCacheKey)
}

func TestCodexTurnIdentity_ClientTurnIDKeepsTimestamp(t *testing.T) {
	clientTurn := uuid.Must(uuid.NewV7()).String()
	ev := collectCodexTurnEvidence(map[string]any{
		"turn_id":                  clientTurn,
		openAIWSTurnMetadataHeader: `{"turn_id":"` + clientTurn + `","turn_started_at_unix_ms":1792601171792}`,
	}, http.Header{}, gjson.Result{})
	id, started := resolveCodexTurnIdentity(newTestTurnClock(), "seed", "thread", ev)
	requireUUIDv7(t, id)
	require.NotEqual(t, clientTurn, id, "client turn id is scoped, not forwarded")
	require.EqualValues(t, 1792601171792, started)
	again, _ := resolveCodexTurnIdentity(newTestTurnClock(), "seed", "thread", ev)
	require.Equal(t, id, again, "derivation is deterministic across processes")
}

func TestCodexSessionIdentity_PerConversationV7(t *testing.T) {
	clock := newTestTurnClock()
	clientSession := uuid.Must(uuid.NewV7()).String()
	s1 := resolveCodexSessionIdentity(clock, "seed", clientSession)
	requireUUIDv7(t, s1)
	require.Equal(t, codexUUIDv7Millis(clientSession), codexUUIDv7Millis(s1), "client v7 creation time is preserved")
	require.Equal(t, s1, resolveCodexSessionIdentity(clock, "seed", clientSession))
	s2 := resolveCodexSessionIdentity(clock, "seed", "a1b2c3d4e5f60718")
	requireUUIDv7(t, s2)
	require.NotEqual(t, s1, s2)
	require.NotEqual(t, s2, resolveCodexSessionIdentity(clock, "other-account-seed", "a1b2c3d4e5f60718"))
}

func TestCodexFullMode_PerConversationThreadsAndStableTurns(t *testing.T) {
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Extra: map[string]any{codexFingerprintSeedExtraKey: "6f1b6c2e-3a4d-4f5e-8a9b-0c1d2e3f4a5b", codexFingerprintModeExtraKey: "full"}}
	sessA := uuid.Must(uuid.NewV7()).String()
	sessB := uuid.Must(uuid.NewV7()).String()
	evTurn := codexTurnEvidence{InputTurnSeed: "turn-1"}
	a1 := resolveCodexFingerprintIDsWithWindow(account, sessA, codexFingerprintFull, "", evTurn)
	a2 := resolveCodexFingerprintIDsWithWindow(account, sessA, codexFingerprintFull, "", evTurn)
	b1 := resolveCodexFingerprintIDsWithWindow(account, sessB, codexFingerprintFull, "", evTurn)
	require.NotNil(t, a1)
	require.Equal(t, a1.installationID, b1.installationID, "one device per account")
	require.NotEqual(t, a1.sessionID, b1.sessionID, "each downstream conversation is its own thread")
	require.Equal(t, a1.sessionID, a1.threadID, "root thread: thread_id == session_id")
	requireUUIDv7(t, a1.sessionID)
	require.Equal(t, a1.turnID, a2.turnID, "same turn across requests")
	require.Equal(t, a1.turnStartedAtUnixMs, a2.turnStartedAtUnixMs)
	require.Equal(t, a1.threadID+":0", a1.windowID)

	// A cache key alone is not proof of a session or its default cache policy.
	p := resolveCodexFingerprintIDsWithWindow(account, "", codexFingerprintFull, "", codexTurnEvidence{PromptCacheKey: "cache-key-1"})
	requireUUIDv7(t, p.sessionID)
	require.False(t, shouldRewriteCodexFingerprintPromptCacheKey(p, "cache-key-1"))
	q := resolveCodexFingerprintIDsWithWindow(account, "", codexFingerprintFull, "", codexTurnEvidence{PromptCacheKey: "cache-key-2"})
	require.NotEqual(t, p.sessionID, q.sessionID)
	sameCache := resolveCodexFingerprintIDsWithWindow(account, "", codexFingerprintFull, "", codexTurnEvidence{PromptCacheKey: "cache-key-1"})
	require.NotEqual(t, p.sessionID, sameCache.sessionID)
}

func TestCodexTurnIdentity_CanonicalMetadataAndTimestampEvidence(t *testing.T) {
	headers := http.Header{}
	headers.Set(openAIWSTurnMetadataHeader, `{"session_id":"header-session","thread_id":"header-thread","turn_id":"stale-turn","turn_started_at_unix_ms":1}`)
	metadata := map[string]any{
		"session_id": "flat-session", "thread_id": "flat-thread", "turn_id": "stale-turn",
		"turn_started_at_unix_ms":  1,
		openAIWSTurnMetadataHeader: `{"session_id":"canonical-session","thread_id":"canonical-thread","turn_id":"` + convTestTurn + `"}`,
	}
	ev := collectCodexTurnEvidence(metadata, headers, gjson.Result{})
	require.Equal(t, "canonical-session", ev.ClientSessionID)
	require.Equal(t, "canonical-thread", ev.ClientThreadID)
	require.Equal(t, convTestTurn, ev.ClientTurnID)
	require.Zero(t, ev.ClientTurnStartedAtMs, "a different turn's timestamp must not be borrowed")
	_, started := resolveCodexTurnIdentity(newTestTurnClock(), "seed", "thread", ev)
	require.Equal(t, codexUUIDv7Millis(convTestTurn), started)

	metadata[openAIWSTurnMetadataHeader] = `{"turn_id":"` + convTestTurn + `","turn_started_at_unix_ms":"1792601171792"}`
	ev = collectCodexTurnEvidence(metadata, headers, gjson.Result{})
	require.EqualValues(t, 1792601171792, ev.ClientTurnStartedAtMs)

	id, started := resolveCodexTurnIdentity(newTestTurnClock(), "seed", "thread", codexTurnEvidence{
		RequestID: convTestTurn, ClientTurnStartedAtMs: 1792601171792,
	})
	require.EqualValues(t, 1792601171792, started)
	require.Equal(t, started, codexUUIDv7Millis(id))
}
