package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// These errors deliberately contain no request, credential, or cache contents.
var (
	ErrNativeReasoningReplayUnavailable = errors.New("native reasoning replay unavailable")
	ErrNativeReasoningReplayInvalid     = errors.New("native reasoning replay ineligible")
	ErrNativeReasoningReplayCapacity    = errors.New("native reasoning replay capacity exceeded")
	ErrNativeReasoningReplayAmbiguous   = errors.New("native reasoning replay ambiguous")
	ErrNativeReasoningReplayCorrupt     = errors.New("native reasoning replay corrupt")
)

// Scope must come from authenticated and selected upstream state. In particular,
// CredentialIdentity is an opaque identity/revision of the actual credential,
// not client metadata, an account label, or a client-supplied account ID. It must
// change when that credential's owner changes, even if the local account ID does
// not. A caller unable to establish any of these boundaries must leave replay off.
type NativeReasoningReplayScope struct {
	UserID             int64  `json:"user_id"`
	APIKeyID           int64  `json:"api_key_id"`
	SessionID          string `json:"session_id"`
	Model              string `json:"model"`
	CredentialIdentity string `json:"credential_identity"`
}

// Parameters is the final semantic request configuration, including instructions,
// tools, tool choice, reasoning and model parameters. Only input and demonstrably
// per-transport fields may be excluded by the production adapter. The engine does
// not guess an allowlist, since dropping an unknown semantic field is unsafe.
type NativeReasoningReplayRequest struct {
	Scope      NativeReasoningReplayScope
	Messages   []json.RawMessage
	Parameters json.RawMessage
}

type NativeReasoningReplayCompletion struct {
	Response json.RawMessage
	// SourceInput is the final native input array actually sent upstream for
	// this response, after normalization and any earlier native replay.
	SourceInput        json.RawMessage
	DeliveredAssistant json.RawMessage
	DeliveredComplete  bool
	ToolNameReverse    map[string]string
}

// Compatibility and the adjacent anchors are from the real apicompat conversion
// before upstream-specific transforms. The forwarding adapter must track these
// spans through its transforms, then splice Output AFTER tool-name/ID rewriting.
// It must also keep corresponding tool outputs paired with the native call_id.
// No substring search or nearest-message heuristic is safe here.
type NativeReasoningReplayReplacement struct {
	MessageIndex              int
	Output                    []json.RawMessage
	Assistant                 json.RawMessage
	ToolNameReverse           map[string]string
	Compatibility             []json.RawMessage
	CompatibilityStart        int
	CompatibilityEnd          int
	Before                    json.RawMessage
	After                     json.RawMessage
	ExpectedSourceInputDigest string
	Reference                 NativeReasoningReplayReference
}

type NativeReasoningReplayReference struct {
	Key         string
	Digest      string
	ScopeDigest string
}

type NativeReasoningReplayLimits struct {
	TTL               time.Duration
	IOTimeout         time.Duration
	MaxEntryBytes     int
	MaxEntriesPerUser int
	MaxEntriesGlobal  int
	MaxLookups        int
	MaxHistoryBytes   int
	MaxOutputItems    int
}

// Limits also bound the quota indexes and the worst-case stored payload bytes:
// 16 MiB per user and 256 MiB globally, before Redis's bounded metadata overhead.
func DefaultNativeReasoningReplayLimits() NativeReasoningReplayLimits {
	return NativeReasoningReplayLimits{
		TTL: time.Hour, IOTimeout: 100 * time.Millisecond,
		MaxEntryBytes: 256 << 10, MaxEntriesPerUser: 64, MaxEntriesGlobal: 1024,
		MaxLookups: 32, MaxHistoryBytes: 4 << 20, MaxOutputItems: 128,
	}
}

// Normalize permits smaller operational limits; hard ceilings cannot be raised
// by a request or a bad configuration. Zero values use the initial defaults.
func (l NativeReasoningReplayLimits) Normalize() NativeReasoningReplayLimits {
	d := DefaultNativeReasoningReplayLimits()
	if l.TTL <= 0 || l.TTL > d.TTL {
		l.TTL = d.TTL
	}
	if l.IOTimeout <= 0 || l.IOTimeout > d.IOTimeout {
		l.IOTimeout = d.IOTimeout
	}
	capInt := func(v, max int) int {
		if v <= 0 || v > max {
			return max
		}
		return v
	}
	l.MaxEntryBytes = capInt(l.MaxEntryBytes, d.MaxEntryBytes)
	l.MaxEntriesPerUser = capInt(l.MaxEntriesPerUser, d.MaxEntriesPerUser)
	l.MaxEntriesGlobal = capInt(l.MaxEntriesGlobal, d.MaxEntriesGlobal)
	l.MaxLookups = capInt(l.MaxLookups, d.MaxLookups)
	l.MaxHistoryBytes = capInt(l.MaxHistoryBytes, d.MaxHistoryBytes)
	l.MaxOutputItems = capInt(l.MaxOutputItems, d.MaxOutputItems)
	return l
}

type NativeReasoningReplayValue struct {
	Digest     string
	Ciphertext string
}

type NativeReasoningReplayWrite struct {
	Owner string
	Key   string
	NativeReasoningReplayValue
}

// This optional interface is intentionally separate from GatewayCache and the
// opposite-direction plaintext reasoning cache. Put atomically publishes both
// value and quota indexes, treats equal digests idempotently, and poisons unequal
// digests at the same key for the remainder of its TTL. Get must bound bytes
// before fetching values and treat poisoned keys as misses.
type NativeReasoningReplayStore interface {
	GetNativeReasoningReplay(ctx context.Context, owner string, keys []string, maxBytes int) (map[string]NativeReasoningReplayValue, error)
	PutNativeReasoningReplay(ctx context.Context, value NativeReasoningReplayWrite, limits NativeReasoningReplayLimits) error
}

// Invalidation compares the stable authenticated record digest before poisoning
// the entry. It must never delete or replace a newer record on a stale reject.
type NativeReasoningReplayInvalidationStore interface {
	InvalidateNativeReasoningReplay(ctx context.Context, owner string, refs []NativeReasoningReplayReference) error
}

type NativeReasoningReplay struct {
	store  NativeReasoningReplayStore
	cipher SecretEncryptor
	limits NativeReasoningReplayLimits
	now    func() time.Time
}

func NewNativeReasoningReplay(store NativeReasoningReplayStore, cipher SecretEncryptor, limits NativeReasoningReplayLimits) *NativeReasoningReplay {
	return &NativeReasoningReplay{store: store, cipher: cipher, limits: limits.Normalize(), now: time.Now}
}

type nativeReasoningReplayRecord struct {
	Version           int               `json:"version"`
	ScopeDigest       string            `json:"scope"`
	ParameterDigest   string            `json:"parameters"`
	HistoryDigest     string            `json:"history"`
	SourceInputDigest string            `json:"source_input"`
	Assistant         json.RawMessage   `json:"assistant"`
	Output            []json.RawMessage `json:"output"`
	ToolNameReverse   map[string]string `json:"tool_name_reverse,omitempty"`
}

type nativeReasoningReplayEnvelope struct {
	Record    nativeReasoningReplayRecord `json:"record"`
	Digest    string                      `json:"digest"`
	ExpiresAt int64                       `json:"expires_at"`
}

type nativeReasoningReplayPrepared struct {
	owner, scope, parameters string
	messages                 []json.RawMessage
	keys                     []string
}

func (r *NativeReasoningReplay) prepare(req NativeReasoningReplayRequest) (nativeReasoningReplayPrepared, error) {
	var p nativeReasoningReplayPrepared
	if r == nil || r.store == nil || r.cipher == nil {
		return p, ErrNativeReasoningReplayUnavailable
	}
	s := req.Scope
	if s.UserID <= 0 || s.APIKeyID <= 0 || s.SessionID == "" || s.Model == "" || s.CredentialIdentity == "" ||
		len(s.SessionID) > 4096 || len(s.Model) > 256 || len(s.CredentialIdentity) > 4096 || len(req.Messages) == 0 || len(req.Messages) > 4096 {
		return p, ErrNativeReasoningReplayInvalid
	}
	// Frame every hash field so neither concatenation nor JSON formatting can
	// alias different identities, parameters, message boundaries, or histories.
	scope, _ := json.Marshal(s)
	p.scope = nativeReplayDigest(scope)
	owner, _ := json.Marshal(s.UserID)
	p.owner = nativeReplayDigest(append([]byte("native-replay-owner/v1\x00"), owner...))
	if len(req.Parameters) > r.limits.MaxHistoryBytes {
		return p, ErrNativeReasoningReplayCapacity
	}
	parameters, err := nativeReplayCanonicalObject(req.Parameters)
	if err != nil {
		return p, ErrNativeReasoningReplayInvalid
	}
	if len(parameters) > r.limits.MaxHistoryBytes {
		return p, ErrNativeReasoningReplayCapacity
	}
	p.parameters = nativeReplayDigest(parameters)
	chain := sha256.New()
	nativeReplayHashField(chain, []byte("native-replay-history/v1"))
	nativeReplayHashField(chain, []byte(p.scope))
	nativeReplayHashField(chain, []byte(p.parameters))
	total := len(parameters)
	for _, raw := range req.Messages {
		if total+len(raw) > r.limits.MaxHistoryBytes {
			return p, ErrNativeReasoningReplayCapacity
		}
		message, err := nativeReplayCanonicalMessage(raw)
		if err != nil {
			return p, ErrNativeReasoningReplayInvalid
		}
		total += max(len(raw), len(message))
		if total > r.limits.MaxHistoryBytes {
			return p, ErrNativeReasoningReplayCapacity
		}
		p.messages = append(p.messages, message)
		nativeReplayHashField(chain, message)
		p.keys = append(p.keys, hex.EncodeToString(chain.Sum(nil)))
	}
	return p, nil
}

// Commit is called only after successful client delivery. It never stores
// partial/cancelled/failed output. The exact native array stays opaque; Decrypt
// is used only for our own application envelope, never for encrypted_content.
func (r *NativeReasoningReplay) Commit(ctx context.Context, req NativeReasoningReplayRequest, completion NativeReasoningReplayCompletion) error {
	if !completion.DeliveredComplete || ctx.Err() != nil {
		return ErrNativeReasoningReplayInvalid
	}
	if r == nil {
		return ErrNativeReasoningReplayUnavailable
	}
	if len(completion.Response)+len(completion.DeliveredAssistant) > r.limits.MaxEntryBytes {
		return ErrNativeReasoningReplayCapacity
	}
	if len(completion.SourceInput) > r.limits.MaxHistoryBytes {
		return ErrNativeReasoningReplayCapacity
	}
	sourceDigest, err := NativeReasoningReplayInputDigest(completion.SourceInput)
	if err != nil {
		return err
	}
	assistant, err := nativeReplayCanonicalMessage(completion.DeliveredAssistant)
	if err != nil {
		return ErrNativeReasoningReplayInvalid
	}
	output, err := r.validateCompletion(req.Scope.Model, completion.Response, assistant, completion.ToolNameReverse)
	if err != nil {
		return err
	}
	withAssistant := req
	withAssistant.Messages = append(append([]json.RawMessage(nil), req.Messages...), assistant)
	p, err := r.prepare(withAssistant)
	if err != nil {
		return err
	}
	key := p.keys[len(p.keys)-1]
	record := nativeReasoningReplayRecord{Version: 1, ScopeDigest: p.scope, ParameterDigest: p.parameters,
		HistoryDigest: key, SourceInputDigest: sourceDigest, Assistant: assistant, Output: output, ToolNameReverse: completion.ToolNameReverse}
	rawRecord, err := json.Marshal(record)
	if err != nil {
		return ErrNativeReasoningReplayInvalid
	}
	canonical, err := nativeReplayCanonical(rawRecord)
	if err != nil {
		return ErrNativeReasoningReplayInvalid
	}
	digest := nativeReplayDigest(canonical)
	envelope, _ := json.Marshal(nativeReasoningReplayEnvelope{Record: record, Digest: digest, ExpiresAt: r.now().Add(r.limits.TTL).UnixMilli()})
	if len(envelope) > r.limits.MaxEntryBytes {
		return ErrNativeReasoningReplayCapacity
	}
	ciphertext, err := r.cipher.Encrypt(string(envelope))
	if err != nil {
		return ErrNativeReasoningReplayUnavailable
	}
	if len(ciphertext)+len(digest)+1 > r.limits.MaxEntryBytes {
		return ErrNativeReasoningReplayCapacity
	}
	ioCtx, cancel := context.WithTimeout(ctx, r.limits.IOTimeout)
	defer cancel()
	return r.store.PutNativeReasoningReplay(ioCtx, NativeReasoningReplayWrite{Owner: p.owner, Key: key,
		NativeReasoningReplayValue: NativeReasoningReplayValue{Digest: digest, Ciphertext: ciphertext}}, r.limits)
}

// Restore returns only fully matched, completed assistant rounds. Errors and empty
// results are ordinary fallback outcomes; the adapter keeps its original body.
// At most MaxLookups of the most recent eligible assistant turns are read in a
// single bounded store operation. Every key includes the whole preceding history.
func (r *NativeReasoningReplay) Restore(ctx context.Context, req NativeReasoningReplayRequest) ([]NativeReasoningReplayReplacement, error) {
	p, err := r.prepare(req)
	if err != nil {
		return nil, err
	}
	candidates := nativeReplayRestoreCandidates(p.messages, r.limits.MaxLookups)
	if len(candidates) == 0 {
		return nil, nil
	}
	keys := make([]string, len(candidates))
	for i, index := range candidates {
		keys[i] = p.keys[index]
	}
	ioCtx, cancel := context.WithTimeout(ctx, r.limits.IOTimeout)
	defer cancel()
	values, err := r.store.GetNativeReasoningReplay(ioCtx, p.owner, keys, r.limits.MaxEntryBytes)
	if err != nil {
		return nil, err
	}
	var replacements []NativeReasoningReplayReplacement
	for n := len(candidates) - 1; n >= 0; n-- {
		index := candidates[n]
		value, found := values[p.keys[index]]
		if !found {
			continue
		}
		if len(value.Ciphertext)+len(value.Digest)+1 > r.limits.MaxEntryBytes {
			return nil, ErrNativeReasoningReplayCapacity
		}
		plain, err := r.cipher.Decrypt(value.Ciphertext)
		if err != nil || len(plain) > r.limits.MaxEntryBytes {
			return nil, ErrNativeReasoningReplayCorrupt
		}
		var envelope nativeReasoningReplayEnvelope
		if json.Unmarshal([]byte(plain), &envelope) != nil {
			return nil, ErrNativeReasoningReplayCorrupt
		}
		record := envelope.Record
		if record.Version != 1 || record.ScopeDigest != p.scope || record.ParameterDigest != p.parameters || record.HistoryDigest != p.keys[index] ||
			envelope.ExpiresAt <= r.now().UnixMilli() || !bytes.Equal(record.Assistant, p.messages[index]) {
			return nil, ErrNativeReasoningReplayCorrupt
		}
		raw, _ := json.Marshal(record)
		canonical, err := nativeReplayCanonical(raw)
		if err != nil || nativeReplayDigest(canonical) != value.Digest || envelope.Digest != value.Digest {
			return nil, ErrNativeReasoningReplayCorrupt
		}
		replacements = append(replacements, NativeReasoningReplayReplacement{MessageIndex: index, Assistant: record.Assistant,
			Output: record.Output, ToolNameReverse: record.ToolNameReverse, ExpectedSourceInputDigest: record.SourceInputDigest,
			Reference: NativeReasoningReplayReference{Key: p.keys[index], Digest: value.Digest, ScopeDigest: p.scope}})
	}
	if len(replacements) == 0 {
		return nil, nil
	}
	if err := nativeReplayAttachAnchors(p.messages, replacements); err != nil {
		return nil, err
	}
	return replacements, nil
}

// Invalidate is for an explicit upstream rejection of restored native state.
// The authenticated scope and returned references must be from the same request.
// All failure paths preserve ordinary forwarding and expose no record contents.
func (r *NativeReasoningReplay) Invalidate(ctx context.Context, scope NativeReasoningReplayScope, refs []NativeReasoningReplayReference) error {
	if len(refs) == 0 {
		return nil
	}
	if r == nil || r.store == nil {
		return ErrNativeReasoningReplayUnavailable
	}
	if len(refs) > r.limits.MaxLookups {
		return ErrNativeReasoningReplayCapacity
	}
	// Reuse the same scope validation/framing used for reads and writes.
	p, err := r.prepare(NativeReasoningReplayRequest{Scope: scope, Parameters: json.RawMessage(`{}`), Messages: []json.RawMessage{json.RawMessage(`{"role":"user","content":""}`)}})
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if ref.ScopeDigest != p.scope || len(ref.Key) != 64 || len(ref.Digest) != 64 {
			return ErrNativeReasoningReplayInvalid
		}
	}
	store, ok := r.store.(NativeReasoningReplayInvalidationStore)
	if !ok {
		return ErrNativeReasoningReplayUnavailable
	}
	ioCtx, cancel := context.WithTimeout(ctx, r.limits.IOTimeout)
	defer cancel()
	return store.InvalidateNativeReasoningReplay(ioCtx, p.owner, refs)
}

// NativeReasoningReplayInputDigest fingerprints every item and field of the
// actual native input. A visible-history match alone cannot prove causal state:
// an ancestor may have expired or failed to commit between two client turns.
func NativeReasoningReplayInputDigest(input json.RawMessage) (string, error) {
	if len(input) > DefaultNativeReasoningReplayLimits().MaxHistoryBytes {
		return "", ErrNativeReasoningReplayCapacity
	}
	canonical, err := nativeReplayCanonical(input)
	if err != nil || len(canonical) == 0 || canonical[0] != '[' {
		return "", ErrNativeReasoningReplayInvalid
	}
	if len(canonical) > DefaultNativeReasoningReplayLimits().MaxHistoryBytes {
		return "", ErrNativeReasoningReplayCapacity
	}
	return nativeReplayDigest(canonical), nil
}

func nativeReplayDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func nativeReplayHashField(h hash.Hash, b []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(b)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(b)
}

// Canonicalize object member ordering without converting integer IDs through
// float64. Duplicate JSON members are rejected rather than choosing an owner.
func nativeReplayCanonical(raw []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := nativeReplayDecode(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrNativeReasoningReplayInvalid
	}
	return json.Marshal(v)
}

func nativeReplayDecode(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, ErrNativeReasoningReplayInvalid
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch delim := token.(type) {
	case json.Delim:
		switch delim {
		case '{':
			out := map[string]any{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return nil, err
				}
				name, ok := key.(string)
				if !ok {
					return nil, ErrNativeReasoningReplayInvalid
				}
				if _, duplicate := out[name]; duplicate {
					return nil, ErrNativeReasoningReplayInvalid
				}
				v, err := nativeReplayDecode(d, depth+1)
				if err != nil {
					return nil, err
				}
				out[name] = v
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return nil, ErrNativeReasoningReplayInvalid
			}
			return out, nil
		case '[':
			out := []any{}
			for d.More() {
				v, err := nativeReplayDecode(d, depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return nil, ErrNativeReasoningReplayInvalid
			}
			return out, nil
		default:
			return nil, ErrNativeReasoningReplayInvalid
		}
	default:
		return token, nil
	}
}

func nativeReplayCanonicalObject(raw []byte) ([]byte, error) {
	b, err := nativeReplayCanonical(raw)
	if err != nil || len(b) == 0 || b[0] != '{' {
		return nil, ErrNativeReasoningReplayInvalid
	}
	return b, nil
}

func nativeReplayCanonicalMessage(raw []byte) ([]byte, error) {
	b, err := nativeReplayCanonicalObject(raw)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	var role string
	if json.Unmarshal(m["role"], &role) != nil {
		return nil, ErrNativeReasoningReplayInvalid
	}
	switch role {
	case "system", "developer", "user", "assistant", "tool":
	default:
		return nil, ErrNativeReasoningReplayInvalid
	}
	if _, legacy := m["function_call"]; legacy {
		return nil, ErrNativeReasoningReplayInvalid
	}
	// These optional empty assistant fields carry no content. Real clients use
	// all three spellings; normalize only these known semantic equivalences.
	if role == "assistant" {
		for _, key := range []string{"content", "reasoning_content", "refusal", "name"} {
			if bytes.Equal(m[key], []byte("null")) || bytes.Equal(m[key], []byte(`""`)) {
				delete(m, key)
			}
		}
		if bytes.Equal(m["tool_calls"], []byte("[]")) || bytes.Equal(m["tool_calls"], []byte("null")) {
			delete(m, "tool_calls")
		}
	}
	return json.Marshal(m)
}

// Tool turns require exactly one result per call before another assistant/user
// turn. Message-only turns require a real, nonempty compatibility fragment and
// cannot consume a following unpaired tool result. Both use the same history key.
func nativeReplayRestoreCandidates(messages []json.RawMessage, maxLookups int) []int {
	// Parse once. Re-scanning a long conversation for every assistant turn can
	// otherwise make a bounded byte budget quadratic in the number of messages.
	typed := make([]apicompat.ChatMessage, len(messages))
	callCounts, resultCounts := map[string]int{}, map[string]int{}
	for i, raw := range messages {
		if json.Unmarshal(raw, &typed[i]) != nil {
			return nil
		}
		for _, call := range typed[i].ToolCalls {
			callCounts[call.ID]++
		}
		if typed[i].Role == "tool" {
			resultCounts[typed[i].ToolCallID]++
		}
	}
	var candidates []int
	for index := len(typed) - 1; index >= 0 && len(candidates) < maxLookups; index-- {
		assistant := typed[index]
		if assistant.Role != "assistant" {
			continue
		}
		if len(assistant.ToolCalls) == 0 {
			if index+1 < len(typed) && (typed[index+1].Role == "tool" || typed[index+1].Role == "function") {
				continue
			}
			if nativeReplayVisibleAssistant(assistant) {
				candidates = append(candidates, index)
			}
			continue
		}
		pending := make(map[string]bool, len(assistant.ToolCalls))
		valid := true
		for _, call := range assistant.ToolCalls {
			if call.ID == "" || call.Type != "function" || callCounts[call.ID] != 1 || resultCounts[call.ID] != 1 {
				valid = false
				break
			}
			pending[call.ID] = true
		}
		if !valid {
			continue
		}
		seen := make(map[string]bool, len(pending))
		for i := index + 1; i < len(typed) && typed[i].Role == "tool"; i++ {
			id := typed[i].ToolCallID
			if !pending[id] || seen[id] {
				valid = false
				break
			}
			seen[id] = true
		}
		if valid && len(seen) == len(pending) {
			candidates = append(candidates, index)
		}
	}
	return candidates
}

func nativeReplayVisibleAssistant(message apicompat.ChatMessage) bool {
	if message.Role != "assistant" || len(message.ToolCalls) != 0 || message.FunctionCall != nil {
		return false
	}
	converted, err := apicompat.ChatCompletionsToResponses(&apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{message}})
	if err != nil {
		return false
	}
	var items []struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(converted.Input, &items) != nil || len(items) == 0 {
		return false
	}
	for _, item := range items {
		if item.Type != "message" || item.Role != "assistant" {
			return false
		}
	}
	return true
}

func nativeReplayAttachAnchors(messages []json.RawMessage, replacements []NativeReasoningReplayReplacement) error {
	var all []json.RawMessage
	starts := make([]int, len(messages))
	ends := make([]int, len(messages))
	for i, raw := range messages {
		var message apicompat.ChatMessage
		if json.Unmarshal(raw, &message) != nil {
			return ErrNativeReasoningReplayInvalid
		}
		converted, err := apicompat.ChatCompletionsToResponses(&apicompat.ChatCompletionsRequest{Messages: []apicompat.ChatMessage{message}})
		if err != nil {
			return ErrNativeReasoningReplayInvalid
		}
		var items []json.RawMessage
		if json.Unmarshal(converted.Input, &items) != nil {
			return ErrNativeReasoningReplayInvalid
		}
		starts[i] = len(all)
		all = append(all, items...)
		ends[i] = len(all)
	}
	for i := range replacements {
		r := &replacements[i]
		start, end := starts[r.MessageIndex], ends[r.MessageIndex]
		if start == end {
			return ErrNativeReasoningReplayInvalid
		}
		r.CompatibilityStart, r.CompatibilityEnd = start, end
		r.Compatibility = all[start:end]
		if start > 0 {
			r.Before = all[start-1]
		}
		if end < len(all) {
			r.After = all[end]
		}
	}
	return nil
}
