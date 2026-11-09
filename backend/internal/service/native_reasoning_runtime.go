package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	SettingKeyNativeReasoningReplay    = "openai_native_reasoning_replay_config"
	NativeReasoningReplayModeOff       = "off"
	NativeReasoningReplayModeObserve   = "observe"
	NativeReasoningReplayModeReplay    = "replay"
	nativeReasoningPolicyTTL           = 5 * time.Second
	nativeReasoningReplayAllUsersModel = "gpt-6.1-sol"
)

// The setting is server-owned. No request body/header can enable replay or
// choose its user scope. An empty allowlist never means every user.
// AllUsersModels is an explicit, narrowly validated model authorization.
type NativeReasoningReplayRuntimeConfig struct {
	Mode           string             `json:"mode"`
	UserIDs        []int64            `json:"user_ids"`
	AllUsersModels []string           `json:"all_users_models,omitempty"`
	ModelUserIDs   map[string][]int64 `json:"model_user_ids,omitempty"`
	ExpiresAt      time.Time          `json:"expires_at,omitempty"`
}

type NativeReasoningReplayRuntimeReader interface {
	NativeReasoningReplayPolicy(context.Context) (NativeReasoningReplayRuntimeConfig, error)
}

type nativeReasoningRuntimeSnapshot struct {
	config NativeReasoningReplayRuntimeConfig
	until  time.Time
}

type nativeReasoningRuntime struct {
	reader       NativeReasoningReplayRuntimeReader
	engine       *NativeReasoningReplay
	limits       NativeReasoningReplayLimits
	snapshot     atomic.Pointer[nativeReasoningRuntimeSnapshot]
	mu           sync.Mutex
	rejected     nativeReasoningRejected
	publications nativeReasoningPublications
}

// A local barrier prevents a failed Redis poison write from reintroducing an
// already rejected record. Keys include its immutable digest, so a later valid
// record is not confused with the rejected one. Overflow disables only this
// optimization until all possibly unremembered entries have expired.
type nativeReasoningRejected struct {
	mu            sync.Mutex
	entries       map[NativeReasoningReplayReference]time.Time
	overflowUntil time.Time
}

func (r *nativeReasoningRuntime) rejectReferences(refs []NativeReasoningReplayReference, now time.Time) {
	if r == nil || len(refs) == 0 {
		return
	}
	r.rejected.mu.Lock()
	defer r.rejected.mu.Unlock()
	if r.rejected.entries == nil {
		r.rejected.entries = make(map[NativeReasoningReplayReference]time.Time)
	}
	for ref, until := range r.rejected.entries {
		if !now.Before(until) {
			delete(r.rejected.entries, ref)
		}
	}
	until := now.Add(r.limits.TTL)
	for _, ref := range refs {
		if _, ok := r.rejected.entries[ref]; !ok && len(r.rejected.entries) >= r.limits.MaxEntriesGlobal {
			if until.After(r.rejected.overflowUntil) {
				r.rejected.overflowUntil = until
			}
			continue
		}
		r.rejected.entries[ref] = until
	}
}

func (r *nativeReasoningRuntime) allowedReplacements(values []NativeReasoningReplayReplacement, now time.Time) []NativeReasoningReplayReplacement {
	if r == nil || len(values) == 0 {
		return values
	}
	r.rejected.mu.Lock()
	defer r.rejected.mu.Unlock()
	if now.Before(r.rejected.overflowUntil) {
		return nil
	}
	if len(r.rejected.entries) == 0 {
		return values
	}
	out := make([]NativeReasoningReplayReplacement, 0, len(values))
	for _, value := range values {
		if until, blocked := r.rejected.entries[value.Reference]; blocked {
			if now.Before(until) {
				continue
			}
			delete(r.rejected.entries, value.Reference)
		}
		out = append(out, value)
	}
	return out
}

func normalizeNativeReasoningPolicy(value NativeReasoningReplayRuntimeConfig) (NativeReasoningReplayRuntimeConfig, error) {
	value.Mode = strings.TrimSpace(strings.ToLower(value.Mode))
	if value.Mode == "" {
		value.Mode = NativeReasoningReplayModeOff
	}
	switch value.Mode {
	case NativeReasoningReplayModeOff, NativeReasoningReplayModeObserve, NativeReasoningReplayModeReplay:
	default:
		return NativeReasoningReplayRuntimeConfig{}, errors.New("invalid native reasoning replay mode")
	}
	if len(value.UserIDs) > 256 {
		return NativeReasoningReplayRuntimeConfig{}, errors.New("native reasoning replay allowlist exceeds limit")
	}
	ids := append([]int64(nil), value.UserIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	unique := ids[:0]
	for _, id := range ids {
		if id <= 0 {
			return NativeReasoningReplayRuntimeConfig{}, errors.New("invalid native reasoning replay user ID")
		}
		if len(unique) == 0 || unique[len(unique)-1] != id {
			unique = append(unique, id)
		}
	}
	value.UserIDs = unique
	if len(value.ModelUserIDs) > 1 {
		return NativeReasoningReplayRuntimeConfig{}, errors.New("native reasoning replay model user scope exceeds limit")
	}
	if value.ModelUserIDs != nil {
		modelUsers := make(map[string][]int64, len(value.ModelUserIDs))
		for model, userIDs := range value.ModelUserIDs {
			if model != nativeReasoningReplayAllUsersModel || len(userIDs) > 256 {
				return NativeReasoningReplayRuntimeConfig{}, errors.New("invalid native reasoning replay model user scope")
			}
			ids := append([]int64(nil), userIDs...)
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			unique := ids[:0]
			for _, id := range ids {
				if id <= 0 {
					return NativeReasoningReplayRuntimeConfig{}, errors.New("invalid native reasoning replay model user ID")
				}
				if len(unique) == 0 || unique[len(unique)-1] != id {
					unique = append(unique, id)
				}
			}
			modelUsers[model] = unique
		}
		value.ModelUserIDs = modelUsers
	}
	if len(value.AllUsersModels) > 1 {
		return NativeReasoningReplayRuntimeConfig{}, errors.New("native reasoning replay model scope exceeds limit")
	}
	for _, model := range value.AllUsersModels {
		if model != nativeReasoningReplayAllUsersModel {
			return NativeReasoningReplayRuntimeConfig{}, errors.New("invalid native reasoning replay model scope")
		}
	}
	value.AllUsersModels = append([]string(nil), value.AllUsersModels...)
	return value, nil
}

func (s *SettingService) NativeReasoningReplayPolicy(ctx context.Context) (NativeReasoningReplayRuntimeConfig, error) {
	off := NativeReasoningReplayRuntimeConfig{Mode: NativeReasoningReplayModeOff}
	if s == nil || s.settingRepo == nil {
		return off, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyNativeReasoningReplay)
	if errors.Is(err, ErrSettingNotFound) {
		return off, nil
	}
	if err != nil {
		return off, err
	}
	if strings.TrimSpace(raw) == "" {
		return off, nil
	}
	if len(raw) > 16<<10 {
		return off, errors.New("native reasoning replay setting exceeds limit")
	}
	canonical, err := nativeReplayCanonicalObject([]byte(raw))
	if err != nil {
		return off, errors.New("invalid native reasoning replay setting object")
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	var value NativeReasoningReplayRuntimeConfig
	if err := decoder.Decode(&value); err != nil {
		return off, errors.New("invalid native reasoning replay setting")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return off, errors.New("invalid native reasoning replay setting suffix")
	}
	return normalizeNativeReasoningPolicy(value)
}

func nativeReasoningLimitsFromConfig(cfg *config.Config) NativeReasoningReplayLimits {
	if cfg == nil {
		return DefaultNativeReasoningReplayLimits()
	}
	c := cfg.Gateway.NativeReasoningReplay
	return (NativeReasoningReplayLimits{
		TTL:           time.Duration(c.TTLSeconds) * time.Second,
		IOTimeout:     time.Duration(c.IOTimeoutMilliseconds) * time.Millisecond,
		MaxEntryBytes: c.MaxEntryBytes, MaxEntriesPerUser: c.MaxEntriesPerUser,
		MaxEntriesGlobal: c.MaxEntriesGlobal, MaxLookups: c.MaxLookups,
		MaxHistoryBytes: c.MaxHistoryBytes, MaxOutputItems: c.MaxOutputItems,
	}).Normalize()
}

func newNativeReasoningReplayCipher(cfg *config.Config) SecretEncryptor {
	if cfg == nil || strings.TrimSpace(cfg.JWT.Secret) == "" {
		return nil
	}
	// Reuse the established AES-GCM implementation with a separate key domain.
	return &liveAttestationAES{key: sha256.Sum256([]byte("sub2api/native-reasoning-replay/v1\x00" + cfg.JWT.Secret))}
}

// SetNativeReasoningReplayRuntime attaches trusted dependencies at bootstrap.
// Operational mode changes must go through the reader, not concurrent setters.
func (s *OpenAIGatewayService) SetNativeReasoningReplayRuntime(reader NativeReasoningReplayRuntimeReader, engine *NativeReasoningReplay) {
	if s == nil {
		return
	}
	if reader == nil || engine == nil {
		s.nativeReplayRuntime = nil
		return
	}
	s.nativeReplayRuntime = &nativeReasoningRuntime{reader: reader, engine: engine, limits: engine.limits}
}

func (s *OpenAIGatewayService) initNativeReasoningReplay() {
	if s == nil || s.settingService == nil {
		return
	}
	store, ok := s.cache.(NativeReasoningReplayStore)
	if !ok {
		return
	}
	cipher := newNativeReasoningReplayCipher(s.cfg)
	if cipher == nil {
		return
	}
	s.SetNativeReasoningReplayRuntime(s.settingService, NewNativeReasoningReplay(store, cipher, nativeReasoningLimitsFromConfig(s.cfg)))
}

func (r *nativeReasoningRuntime) policy(ctx context.Context, now time.Time) NativeReasoningReplayRuntimeConfig {
	off := NativeReasoningReplayRuntimeConfig{Mode: NativeReasoningReplayModeOff}
	if r == nil || r.reader == nil || r.engine == nil {
		return off
	}
	if cached := r.snapshot.Load(); cached != nil && now.Before(cached.until) {
		return cached.config
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cached := r.snapshot.Load(); cached != nil && now.Before(cached.until) {
		return cached.config
	}
	bounded, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	value, err := r.reader.NativeReasoningReplayPolicy(bounded)
	cancel()
	if err == nil {
		value, err = normalizeNativeReasoningPolicy(value)
	}
	if err != nil {
		value = off
	}
	// A failed read turns replay off rather than extending a stale authorization.
	r.snapshot.Store(&nativeReasoningRuntimeSnapshot{config: value, until: now.Add(nativeReasoningPolicyTTL)})
	return value
}

func (r *nativeReasoningRuntime) modeForUser(ctx context.Context, userID int64) string {
	mode, _ := r.modeForUserModel(ctx, userID, "")
	return mode
}

// The boolean marks authorization limited to this exact upstream model. Legacy
// user allowlists retain their existing model scope, including an empty list.
func (r *nativeReasoningRuntime) modeForUserModel(ctx context.Context, userID int64, model string) (string, bool) {
	if r == nil || userID <= 0 {
		return NativeReasoningReplayModeOff, false
	}
	now := time.Now()
	value := r.policy(ctx, now)
	if value.Mode == NativeReasoningReplayModeOff || (!value.ExpiresAt.IsZero() && !now.Before(value.ExpiresAt)) {
		return NativeReasoningReplayModeOff, false
	}
	index := sort.Search(len(value.UserIDs), func(i int) bool { return value.UserIDs[i] >= userID })
	if index < len(value.UserIDs) && value.UserIDs[index] == userID {
		return value.Mode, false
	}
	modelUsers := value.ModelUserIDs[model]
	modelIndex := sort.Search(len(modelUsers), func(i int) bool { return modelUsers[i] >= userID })
	if modelIndex < len(modelUsers) && modelUsers[modelIndex] == userID {
		return value.Mode, true
	}
	for _, allowed := range value.AllUsersModels {
		if model == allowed {
			return value.Mode, true
		}
	}
	return NativeReasoningReplayModeOff, false
}
