package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Codex turn identity.
//
// The official client mints one turn per user submission: turn_id is
// Uuid::now_v7() (= the submission id, core/src/session/turn_context.rs) and
// turn_started_at_unix_ms is the submission time. Every request of that turn —
// the first one and all tool-loop follow-ups — repeats both values
// (testdata/golden/0.163.0: http_fallback_tool_loop / ws_tool_loop requests 2-3
// share turn_id and turn_started_at_unix_ms). A gateway that mints a fresh
// turn_id per HTTP request produces a shape the official client never emits.
//
// Real Codex downstream clients already send a turn_id; it is kept (scoped per
// account by the identity layer). Other clients carry no turn notion, so the
// turn is derived from the conversation: a new user message starts a turn,
// function_call_output follow-ups stay in it.

// codexTurnEvidence is what a request tells us about its turn.
type codexTurnEvidence struct {
	// ClientTurnID is the client's own turn_id (client_metadata, embedded or
	// header x-codex-turn-metadata).
	ClientTurnID string
	// ClientTurnStartedAtMs is the client's turn_started_at_unix_ms.
	ClientTurnStartedAtMs int64
	// InputTurnSeed fingerprints the last user message and its ordinal, so
	// tool-loop follow-ups (which only append after it) map to the same turn.
	InputTurnSeed string
	// PromptCacheKey is the body prompt_cache_key, used as session evidence
	// when the client sends no session id.
	PromptCacheKey string
}

func codexTurnEvidenceFromMetadataString(raw string, ev *codexTurnEvidence) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	meta := gjson.Parse(raw)
	if !meta.IsObject() {
		return
	}
	if ev.ClientTurnID == "" {
		ev.ClientTurnID = strings.TrimSpace(meta.Get("turn_id").String())
	}
	if ev.ClientTurnStartedAtMs == 0 {
		if v := meta.Get("turn_started_at_unix_ms"); v.Type == gjson.Number && v.Int() > 0 {
			ev.ClientTurnStartedAtMs = v.Int()
		}
	}
}

// collectCodexTurnEvidence gathers turn evidence from the client metadata
// (map or gjson form), inbound headers and the request body.
func collectCodexTurnEvidence(clientMetadata any, headers http.Header, body gjson.Result) codexTurnEvidence {
	var ev codexTurnEvidence
	switch cm := clientMetadata.(type) {
	case map[string]any:
		if v, ok := cm["turn_id"].(string); ok {
			ev.ClientTurnID = strings.TrimSpace(v)
		}
		if v, ok := cm[openAIWSTurnMetadataHeader].(string); ok {
			codexTurnEvidenceFromMetadataString(v, &ev)
		}
	case gjson.Result:
		if cm.IsObject() {
			ev.ClientTurnID = strings.TrimSpace(cm.Get("turn_id").String())
			codexTurnEvidenceFromMetadataString(cm.Get(gjsonEscapeKey(openAIWSTurnMetadataHeader)).String(), &ev)
		}
	}
	if headers != nil {
		codexTurnEvidenceFromMetadataString(headers.Get(openAIWSTurnMetadataHeader), &ev)
	}
	if body.Exists() {
		ev.PromptCacheKey = strings.TrimSpace(body.Get("prompt_cache_key").String())
		ev.InputTurnSeed = codexInputTurnSeed(body.Get("input"))
	}
	return ev
}

// collectCodexTurnEvidenceFromMap is collectCodexTurnEvidence for an already
// decoded body (non-passthrough path) without re-encoding the whole body.
func collectCodexTurnEvidenceFromMap(decoded map[string]any, headers http.Header) codexTurnEvidence {
	if decoded == nil {
		return collectCodexTurnEvidence(nil, headers, gjson.Result{})
	}
	ev := collectCodexTurnEvidence(decoded["client_metadata"], headers, gjson.Result{})
	if key, ok := decoded["prompt_cache_key"].(string); ok {
		ev.PromptCacheKey = strings.TrimSpace(key)
	}
	if input, ok := decoded["input"].([]any); ok {
		userCount := 0
		var last any
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok || item["role"] != "user" {
				continue
			}
			if t, _ := item["type"].(string); t != "" && t != "message" {
				continue
			}
			userCount++
			last = item["content"]
		}
		if userCount > 0 {
			canonical, err := json.Marshal(last)
			if err == nil {
				ev.InputTurnSeed = codexTurnSeedDigest(userCount, canonical)
			}
		}
	}
	return ev
}

func codexTurnSeedDigest(userCount int, canonicalContent []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(strconv.Itoa(userCount)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(canonicalContent)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// gjsonEscapeKey escapes gjson path metacharacters in a literal key.
func gjsonEscapeKey(key string) string {
	r := strings.NewReplacer(".", `\.`, "*", `\*`, "?", `\?`)
	return r.Replace(key)
}

// codexInputTurnSeed returns a digest of the last user message plus the
// number of user messages up to it. Content is canonicalized (decoded and
// re-encoded) so map- and raw-encoded bodies of the same request agree.
func codexInputTurnSeed(input gjson.Result) string {
	if !input.IsArray() {
		return ""
	}
	userCount := 0
	var last gjson.Result
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("role").String() == "user" {
			t := item.Get("type").String()
			if t == "" || t == "message" {
				userCount++
				last = item.Get("content")
			}
		}
		return true
	})
	if userCount == 0 {
		return ""
	}
	var canonical []byte
	var decoded any
	if err := json.Unmarshal([]byte(last.Raw), &decoded); err == nil {
		canonical, _ = json.Marshal(decoded)
	} else {
		canonical = []byte(last.Raw)
	}
	return codexTurnSeedDigest(userCount, canonical)
}

// codexTurnClock remembers when a derived turn (or session) was first seen,
// so every request of that turn reports the same start time. In-process and
// bounded: a gateway restart looks like a client restart (new turn times).
type codexTurnClock struct {
	entries sync.Map // key -> codexTurnClockEntry
	inserts atomic.Int64
	ttl     time.Duration
	now     func() time.Time
}

type codexTurnClockEntry struct {
	startedAtMs int64
	lastSeen    atomic.Int64
}

var defaultCodexTurnClock = &codexTurnClock{ttl: 6 * time.Hour, now: time.Now}

const codexTurnClockPruneEvery = 4096

// StartedAt returns the first-seen unix ms for key, recording now if new.
func (c *codexTurnClock) StartedAt(key string) int64 {
	now := c.now()
	nowMs := now.UnixMilli()
	fresh := &codexTurnClockEntry{startedAtMs: nowMs}
	fresh.lastSeen.Store(nowMs)
	raw, loaded := c.entries.LoadOrStore(key, fresh)
	entry, ok := raw.(*codexTurnClockEntry)
	if !ok {
		// Only *codexTurnClockEntry is ever stored; replace anything else.
		c.entries.Store(key, fresh)
		return nowMs
	}
	if loaded {
		entry.lastSeen.Store(nowMs)
		return entry.startedAtMs
	}
	if c.inserts.Add(1)%codexTurnClockPruneEvery == 0 {
		c.prune(now)
	}
	return entry.startedAtMs
}

func (c *codexTurnClock) prune(now time.Time) {
	cutoff := now.Add(-c.ttl).UnixMilli()
	c.entries.Range(func(k, v any) bool {
		if entry, ok := v.(*codexTurnClockEntry); !ok || entry.lastSeen.Load() < cutoff {
			c.entries.Delete(k)
		}
		return true
	})
}

// codexUUIDv7Millis returns the timestamp of a canonical UUIDv7, or 0.
func codexUUIDv7Millis(raw string) int64 {
	parsed, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Version() != 7 {
		return 0
	}
	var ms int64
	for _, b := range parsed[0:6] {
		ms = ms<<8 | int64(b)
	}
	return ms
}

// resolveCodexTurnIdentity returns the outbound (turn_id, turn_started_at_ms)
// for a request in thread threadID. The same Codex turn always yields the same
// pair; a new user message yields a new pair.
func resolveCodexTurnIdentity(clock *codexTurnClock, seed, threadID string, ev codexTurnEvidence) (string, int64) {
	if clock == nil {
		clock = defaultCodexTurnClock
	}
	if ev.ClientTurnID != "" {
		started := ev.ClientTurnStartedAtMs
		if started <= 0 {
			started = codexUUIDv7Millis(ev.ClientTurnID)
		}
		if started <= 0 {
			started = clock.StartedAt("client-turn:" + seed + ":" + ev.ClientTurnID)
		}
		return deriveStableCodexUUIDv7("sub2api:codex-turn-id:v3:"+seed+":"+ev.ClientTurnID, started), started
	}
	turnKey := ev.InputTurnSeed
	if turnKey == "" {
		// No conversation to anchor on: every request is its own turn, as the
		// official client does for a single submission.
		started := clock.now().UnixMilli()
		return uuid.Must(uuid.NewV7()).String(), started
	}
	key := "derived-turn:" + seed + ":" + threadID + ":" + turnKey
	started := clock.StartedAt(key)
	return deriveStableCodexUUIDv7("sub2api:codex-turn-id:v3:"+key, started), started
}

// resolveCodexSessionIdentity returns the outbound session/thread id for one
// downstream conversation: a UUIDv7 that is stable for the conversation and
// distinct across conversations. A client v7 session id keeps its creation
// timestamp; other keys use the first time the gateway saw them.
func resolveCodexSessionIdentity(clock *codexTurnClock, seed, sessionKey string) string {
	if clock == nil {
		clock = defaultCodexTurnClock
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return ""
	}
	created := codexUUIDv7Millis(sessionKey)
	if created <= 0 {
		created = clock.StartedAt("session:" + seed + ":" + sessionKey)
	}
	return deriveStableCodexUUIDv7("sub2api:codex-session-id:v3:"+seed+":"+sessionKey, created)
}
