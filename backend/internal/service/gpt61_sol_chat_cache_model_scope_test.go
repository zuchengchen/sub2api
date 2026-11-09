package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGPT61SolChatCacheModelScopeSetting(t *testing.T) {
	for _, raw := range []string{
		`{"mode":"replay","user_ids":[428],"all_users_models":["gpt-6.1-sol"]}`,
		`{"mode":"observe","all_users_models":["gpt-6.1-sol"]}`,
		`{"mode":"replay","user_ids":[],"all_users_models":[]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s := NewSettingService(&nativeReplaySettingFixture{raw: raw}, nil)
			_, err := s.NativeReasoningReplayPolicy(context.Background())
			require.NoError(t, err)
		})
	}
	for _, raw := range []string{
		`{"mode":"replay","all_users_models":["*"]}`,
		`{"mode":"replay","all_users_models":["gpt-5.5"]}`,
		`{"mode":"replay","all_users_models":[""]}`,
		`{"mode":"replay","all_users_models":["GPT-6.1-SOL"]}`,
		`{"mode":"replay","all_users_models":["gpt-6.1-sol","gpt-6.1-sol"]}`,
		`{"mode":"off","all_users_models":["*"]}`,
		`{"mode":"replay","all_users_models":["gpt-6.1-sol"],"all_users_models":[]}`,
		`{"mode":"replay","all_users_models":["gpt-6.1-sol"],"all_users":true}`,
	} {
		t.Run(raw, func(t *testing.T) {
			s := NewSettingService(&nativeReplaySettingFixture{raw: raw}, nil)
			_, err := s.NativeReasoningReplayPolicy(context.Background())
			require.Error(t, err)
		})
	}
}

func TestGPT61SolChatCacheModelScopeAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, mode    string
		users         []int64
		models        []string
		expired, fail bool
		user          int64
		model         string
		want          string
		restricted    bool
	}{
		{name: "legacy_target", mode: "replay", users: []int64{428}, user: 428, model: "gpt-6.1-sol", want: "replay"},
		{name: "legacy_other", mode: "replay", users: []int64{428}, user: 428, model: "gpt-5.5", want: "replay"},
		{name: "legacy_ordinary_off", mode: "replay", users: []int64{428}, user: 101, model: "gpt-6.1-sol", want: "off"},
		{name: "legacy_empty_off", mode: "replay", user: 101, model: "gpt-6.1-sol", want: "off"},
		{name: "ordinary_one", mode: "replay", users: []int64{428}, models: []string{"gpt-6.1-sol"}, user: 101, model: "gpt-6.1-sol", want: "replay", restricted: true},
		{name: "ordinary_two", mode: "replay", models: []string{"gpt-6.1-sol"}, user: 102, model: "gpt-6.1-sol", want: "replay", restricted: true},
		{name: "other_model", mode: "replay", models: []string{"gpt-6.1-sol"}, user: 101, model: "gpt-5.5", want: "off"},
		{name: "unknown_actual_model", mode: "replay", models: []string{"gpt-6.1-sol"}, user: 101, want: "off"},
		{name: "observe", mode: "observe", models: []string{"gpt-6.1-sol"}, user: 101, model: "gpt-6.1-sol", want: "observe", restricted: true},
		{name: "global_off", mode: "off", users: []int64{428}, models: []string{"gpt-6.1-sol"}, user: 428, model: "gpt-6.1-sol", want: "off"},
		{name: "expired", mode: "replay", users: []int64{428}, models: []string{"gpt-6.1-sol"}, expired: true, user: 428, model: "gpt-6.1-sol", want: "off"},
		{name: "read_failure", mode: "replay", users: []int64{428}, models: []string{"gpt-6.1-sol"}, fail: true, user: 428, model: "gpt-6.1-sol", want: "off"},
		{name: "invalid_policy", mode: "replay", users: []int64{428}, models: []string{"*"}, user: 428, model: "gpt-6.1-sol", want: "off"},
		{name: "invalid_user", mode: "replay", models: []string{"gpt-6.1-sol"}, user: 0, model: "gpt-6.1-sol", want: "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NativeReasoningReplayRuntimeConfig{Mode: tc.mode, UserIDs: tc.users, AllUsersModels: tc.models}
			if tc.expired {
				p.ExpiresAt = time.Now().Add(-time.Second)
			}
			r := &nativeReasoningRuntime{engine: NewNativeReasoningReplay(nil, nil, NativeReasoningReplayLimits{}), reader: nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) {
				if tc.fail {
					return p, errors.New("synthetic policy failure")
				}
				return p, nil
			})}
			mode, restricted := r.modeForUserModel(context.Background(), tc.user, tc.model)
			require.Equal(t, tc.want, mode)
			require.Equal(t, tc.restricted, restricted)
			a := nativeReasoningChatAuthorization{mode: mode, model: tc.model, userID: tc.user, apiKeyID: 500}
			require.Equal(t, tc.want == "replay" && tc.model == "gpt-6.1-sol" && tc.user > 0, a.targetReplayEnabled())
		})
	}
}

func TestGPT61SolChatCacheModelScopeRequestSnapshotAndPrepareGuard(t *testing.T) {
	engine, store, _, _ := nativeReplayFixture(t)
	var reads atomic.Int64
	var failed atomic.Bool
	svc := &OpenAIGatewayService{}
	svc.SetNativeReasoningReplayRuntime(nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) {
		reads.Add(1)
		if failed.Load() {
			return NativeReasoningReplayRuntimeConfig{}, errors.New("synthetic failure")
		}
		return NativeReasoningReplayRuntimeConfig{Mode: "replay", AllUsersModels: []string{"gpt-6.1-sol"}}, nil
	}), engine)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("api_key", &APIKey{ID: 202, UserID: 101})
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "synthetic-owner"}}
	body := []byte(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"synthetic"}]}`)
	auth := svc.resolveNativeReasoningChatAuthorization(c, "gpt-6.1-sol")
	require.True(t, auth.targetReplayEnabled(), "body-only identity evidence must not depend on native header-session admission")
	require.Nil(t, svc.beginNativeReasoningChat(c, account, body, auth), "native replay still requires a supported header session")
	c.Request.Header.Set("session-id", "synthetic-session")
	failed.Store(true)
	svc.nativeReplayRuntime.snapshot.Store(&nativeReasoningRuntimeSnapshot{until: time.Now().Add(-time.Second)})
	n := svc.beginNativeReasoningChat(c, account, body, auth)
	require.NotNil(t, n)
	require.EqualValues(t, 1, reads.Load(), "begin must reuse the request authorization instead of reading policy again")
	badWire := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"synthetic"}]}`)
	require.Equal(t, badWire, n.prepare(context.Background(), c, account, "synthetic-token", badWire))
	require.False(t, n.eligible)
	require.Nil(t, n.publication)
	require.Empty(t, n.references)
	require.Zero(t, store.gets)
	require.EqualValues(t, 1, reads.Load(), "prepare must check its captured model restriction without reauthorizing")
	c.Set("api_key", &APIKey{ID: 203, UserID: 101})
	require.Nil(t, svc.beginNativeReasoningChat(c, account, body, auth), "authorization cannot move to another authenticated Key")
	require.False(t, svc.resolveNativeReasoningChatAuthorization(c, "gpt-6.1-sol").targetReplayEnabled())
	require.EqualValues(t, 2, reads.Load(), "the next request observes the expired snapshot and failed read")
	c.Set("api_key", (*APIKey)(nil))
	require.False(t, svc.resolveNativeReasoningChatAuthorization(c, "gpt-6.1-sol").targetReplayEnabled())
}

func TestGPT61SolChatCacheModelScopeConcurrentPolicy(t *testing.T) {
	var reads atomic.Int64
	r := &nativeReasoningRuntime{engine: NewNativeReasoningReplay(nil, nil, NativeReasoningReplayLimits{}), reader: nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) {
		reads.Add(1)
		return NativeReasoningReplayRuntimeConfig{Mode: "replay", UserIDs: []int64{428}, AllUsersModels: []string{"gpt-6.1-sol"}}, nil
	})}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(user int64) {
			defer wg.Done()
			mode, restricted := r.modeForUserModel(context.Background(), user, "gpt-6.1-sol")
			if mode != "replay" || !restricted {
				t.Errorf("wrong ordinary-user decision: %s %v", mode, restricted)
			}
		}(int64(1000 + i))
	}
	wg.Wait()
	require.EqualValues(t, 1, reads.Load())
}

func TestGPT61SolChatCacheModelScopeCanary(t *testing.T) {
	settings := NewSettingService(&nativeReplaySettingFixture{raw: `{"mode":"replay","user_ids":[428],"model_user_ids":{"gpt-6.1-sol":[358,101,358]}}`}, nil)
	policy, err := settings.NativeReasoningReplayPolicy(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int64{101, 358}, policy.ModelUserIDs["gpt-6.1-sol"])
	for _, tc := range []struct {
		name        string
		user        int64
		model, want string
		restricted  bool
	}{
		{"canary_target", 358, "gpt-6.1-sol", "replay", true},
		{"canary_other_model", 358, "gpt-5.5", "off", false},
		{"ordinary_target", 359, "gpt-6.1-sol", "off", false},
		{"legacy_target", 428, "gpt-6.1-sol", "replay", false},
		{"legacy_other_model", 428, "gpt-5.5", "replay", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &nativeReasoningRuntime{engine: NewNativeReasoningReplay(nil, nil, NativeReasoningReplayLimits{}), reader: nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) { return policy, nil })}
			mode, restricted := r.modeForUserModel(context.Background(), tc.user, tc.model)
			require.Equal(t, tc.want, mode)
			require.Equal(t, tc.restricted, restricted)
		})
	}
	t.Run("promoted_new_user", func(t *testing.T) {
		promoted := policy
		promoted.AllUsersModels = []string{"gpt-6.1-sol"}
		r := &nativeReasoningRuntime{engine: NewNativeReasoningReplay(nil, nil, NativeReasoningReplayLimits{}), reader: nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) { return promoted, nil })}
		mode, restricted := r.modeForUserModel(context.Background(), 9999, "gpt-6.1-sol")
		require.Equal(t, "replay", mode)
		require.True(t, restricted)
	})
	t.Run("empty_scoped_list", func(t *testing.T) {
		empty := policy
		empty.ModelUserIDs = map[string][]int64{"gpt-6.1-sol": {}}
		r := &nativeReasoningRuntime{engine: NewNativeReasoningReplay(nil, nil, NativeReasoningReplayLimits{}), reader: nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) { return empty, nil })}
		mode, _ := r.modeForUserModel(context.Background(), 358, "gpt-6.1-sol")
		require.Equal(t, "off", mode)
	})
	t.Run("snapshot_owns_map_and_slices", func(t *testing.T) {
		original := NativeReasoningReplayRuntimeConfig{Mode: "replay", ModelUserIDs: map[string][]int64{"gpt-6.1-sol": {358, 101, 358}}}
		normalized, err := normalizeNativeReasoningPolicy(original)
		require.NoError(t, err)
		original.ModelUserIDs["gpt-6.1-sol"][0] = 999
		original.ModelUserIDs["*"] = []int64{999}
		require.Equal(t, map[string][]int64{"gpt-6.1-sol": {101, 358}}, normalized.ModelUserIDs)
	})
	t.Run("snapshot_owns_empty_map", func(t *testing.T) {
		original := NativeReasoningReplayRuntimeConfig{Mode: "replay", ModelUserIDs: map[string][]int64{}}
		normalized, err := normalizeNativeReasoningPolicy(original)
		require.NoError(t, err)
		original.ModelUserIDs["gpt-6.1-sol"] = []int64{358}
		require.Empty(t, normalized.ModelUserIDs, "a reader cannot mutate a cached empty scope into an authorization")
	})
	invalid := []NativeReasoningReplayRuntimeConfig{
		{Mode: "replay", ModelUserIDs: map[string][]int64{"*": {358}}},
		{Mode: "replay", ModelUserIDs: map[string][]int64{"gpt-5.5": {358}}},
		{Mode: "replay", ModelUserIDs: map[string][]int64{"gpt-6.1-sol": {0}}},
		{Mode: "replay", ModelUserIDs: map[string][]int64{"gpt-6.1-sol": {-1}}},
		{Mode: "replay", ModelUserIDs: map[string][]int64{"gpt-6.1-sol": make([]int64, 257)}},
		{Mode: "replay", ModelUserIDs: map[string][]int64{"gpt-6.1-sol": {358}, "other": {358}}},
	}
	for _, bad := range invalid {
		_, err := normalizeNativeReasoningPolicy(bad)
		require.Error(t, err)
		bad.UserIDs = []int64{428}
		r := &nativeReasoningRuntime{engine: NewNativeReasoningReplay(nil, nil, NativeReasoningReplayLimits{}), reader: nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) { return bad, nil })}
		mode, _ := r.modeForUserModel(context.Background(), 428, "gpt-6.1-sol")
		require.Equal(t, "off", mode, "invalid model scope closes the full setting")
	}
	for _, raw := range []string{
		`{"mode":"replay","model_user_ids":{"gpt-6.1-sol":[358],"gpt-6.1-sol":[359]}}`,
		`{"mode":"replay","model_user_ids":{"gpt-6.1-sol":[358]},"model_user_ids":{}}`,
	} {
		s := NewSettingService(&nativeReplaySettingFixture{raw: raw}, nil)
		_, err := s.NativeReasoningReplayPolicy(context.Background())
		require.Error(t, err)
	}
}
