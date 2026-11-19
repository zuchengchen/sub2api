package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type nativeReplayPolicyReaderFunc func(context.Context) (NativeReasoningReplayRuntimeConfig, error)

func (f nativeReplayPolicyReaderFunc) NativeReasoningReplayPolicy(ctx context.Context) (NativeReasoningReplayRuntimeConfig, error) {
	return f(ctx)
}

type nativeReplaySettingFixture struct {
	SettingRepository
	raw string
	err error
}

func (f *nativeReplaySettingFixture) GetValue(_ context.Context, key string) (string, error) {
	if key != SettingKeyNativeReasoningReplay {
		return "", errors.New("unexpected setting key")
	}
	return f.raw, f.err
}

func TestNativeReasoningReplayRuntimeSettingValidation(t *testing.T) {
	for _, raw := range []string{`{"mode":"typo","user_ids":[428]}`, `{"mode":"replay","user_ids":[0]}`, `{"mode":"replay","user_ids":[-1]}`, `{"mode":"off","mode":"replay","user_ids":[428]}`, `{"mode":"replay","user_ids":[428],"unknown":"secret"}`, `{"mode":"replay","user_ids":[428]} {}`, `[]`, strings.Repeat(" ", 16<<10) + `{}`} {
		s := NewSettingService(&nativeReplaySettingFixture{raw: raw}, nil)
		_, err := s.NativeReasoningReplayPolicy(context.Background())
		require.Error(t, err)
	}
	s := NewSettingService(&nativeReplaySettingFixture{raw: `{"mode":" REPLAY ","user_ids":[429,428,428]}`}, nil)
	got, err := s.NativeReasoningReplayPolicy(context.Background())
	require.NoError(t, err)
	require.Equal(t, NativeReasoningReplayModeReplay, got.Mode)
	require.Equal(t, []int64{428, 429}, got.UserIDs)
	s = NewSettingService(&nativeReplaySettingFixture{err: ErrSettingNotFound}, nil)
	got, err = s.NativeReasoningReplayPolicy(context.Background())
	require.NoError(t, err)
	require.Equal(t, NativeReasoningReplayModeOff, got.Mode)
}

func TestNativeReasoningReplayRuntimeGPT62ModelGrants(t *testing.T) {
	raw := `{
		"mode":"replay","user_ids":[428],"all_users_models":["gpt-6.1-sol"],
		"model_user_ids":{"gpt-6.1-sol":[428],"gpt-6.2-astra":[428,358,358],"gpt-6.2-sol":[358]}
	}`
	reader := NewSettingService(&nativeReplaySettingFixture{raw: raw}, nil)
	config, err := reader.NativeReasoningReplayPolicy(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int64{358, 428}, config.ModelUserIDs["gpt-6.2-astra"])
	runtime := &nativeReasoningRuntime{engine: NewNativeReasoningReplay(nil, nil, NativeReasoningReplayLimits{}), reader: reader}
	for _, tc := range []struct {
		user       int64
		model      string
		mode       string
		restricted bool
	}{
		{358, "gpt-6.2-astra", "replay", true},
		{358, "gpt-6.2-sol", "replay", true},
		{358, "gpt-5.5", "off", false},
		{359, "gpt-6.2-sol", "off", false},
		{359, "gpt-6.1-sol", "replay", true},
		{428, "gpt-6.2-astra", "replay", true}, // Explicit opt-in for an existing legacy user.
		{428, "gpt-6.2-sol", "replay", false},
		{428, "gpt-6.1-sol", "replay", false},
	} {
		mode, restricted := runtime.modeForUserModel(context.Background(), tc.user, tc.model)
		require.Equal(t, tc.mode, mode, "user %d, model %s", tc.user, tc.model)
		require.Equal(t, tc.restricted, restricted, "user %d, model %s", tc.user, tc.model)
	}
	for _, invalid := range []string{
		`{"mode":"replay","all_users_models":["gpt-6.2-astra"]}`,
		`{"mode":"replay","all_users_models":["gpt-6.2-sol"]}`,
		`{"mode":"replay","model_user_ids":{"gpt-6.2-sol-high":[358]}}`,
		`{"mode":"replay","model_user_ids":{"GPT-6.2-ASTRA":[358]}}`,
	} {
		service := NewSettingService(&nativeReplaySettingFixture{raw: invalid}, nil)
		_, err := service.NativeReasoningReplayPolicy(context.Background())
		require.Error(t, err, invalid)
	}
}

func TestNativeReasoningReplayRuntimeAllUsersGPTModels(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		setting     string
		user        int64
		model, mode string
		restricted  bool
	}{
		{`{"mode":"replay","all_users_gpt_models":true}`, 1, "gpt-4o", "replay", true},
		{`{"mode":"replay","all_users_gpt_models":true}`, 987654, "gpt-6.1-terra", "replay", true},
		{`{"mode":"observe","all_users_gpt_models":true}`, 2, "gpt-6.2-sol", "observe", true},
		{`{"mode":"replay","user_ids":[2],"all_users_gpt_models":true}`, 2, "gpt-5.5", "replay", true},
		{`{"mode":"replay","user_ids":[2],"all_users_gpt_models":true}`, 2, "other", "replay", false},
		{`{"mode":"replay","all_users_gpt_models":true}`, 0, "gpt-5.5", "off", false},
		{`{"mode":"replay","all_users_gpt_models":true}`, 3, "grok-4.5", "off", false},
		{`{"mode":"replay","all_users_gpt_models":false}`, 3, "gpt-6.2-astra", "off", false},
		{`{"mode":"off","all_users_gpt_models":true}`, 2, "gpt-5.5", "off", false},
		{`{"mode":"replay","all_users_gpt_models":true,"expires_at":"2000-01-01T00:00:00Z"}`, 2, "gpt-5.5", "off", false},
	} {
		reader := NewSettingService(&nativeReplaySettingFixture{raw: tc.setting}, nil)
		r := &nativeReasoningRuntime{engine: NewNativeReasoningReplay(nil, nil, NativeReasoningReplayLimits{}), reader: reader}
		mode, restricted := r.modeForUserModel(ctx, tc.user, tc.model)
		require.Equal(t, tc.mode, mode, "%s %s", tc.setting, tc.model)
		require.Equal(t, tc.restricted, restricted, "%s %s", tc.setting, tc.model)
	}
	for _, raw := range []string{
		`{"mode":"replay","all_users_gpt_models":"true"}`,
		`{"mode":"replay","all_users_gpt_models":null}`,
		`{"mode":"replay","ALL_USERS_GPT_MODELS":true}`,
		`{"mode":"replay","all_users_gpt_models":false,"ALL_USERS_GPT_MODELS":true}`,
		`{"mode":"replay","all_users_gpt_models":true,"all_users_gpt_models":false}`,
	} {
		reader := NewSettingService(&nativeReplaySettingFixture{raw: raw}, nil)
		_, err := reader.NativeReasoningReplayPolicy(ctx)
		require.Error(t, err)
	}
}

func TestNativeReasoningReplayRuntimeAuthorizationCacheAndFailure(t *testing.T) {
	var reads atomic.Int64
	var failed atomic.Bool
	reader := nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) {
		reads.Add(1)
		if failed.Load() {
			return NativeReasoningReplayRuntimeConfig{}, errors.New("unavailable")
		}
		return NativeReasoningReplayRuntimeConfig{Mode: NativeReasoningReplayModeReplay, UserIDs: []int64{428}}, nil
	})
	s := &OpenAIGatewayService{}
	s.SetNativeReasoningReplayRuntime(reader, NewNativeReasoningReplay(nil, nil, DefaultNativeReasoningReplayLimits()))
	r := s.nativeReplayRuntime
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.Equal(t, NativeReasoningReplayModeReplay, r.modeForUser(context.Background(), 428))
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, reads.Load())
	require.Equal(t, NativeReasoningReplayModeOff, r.modeForUser(context.Background(), 429))
	failed.Store(true)
	r.snapshot.Store(&nativeReasoningRuntimeSnapshot{until: time.Now().Add(-time.Second)})
	require.Equal(t, NativeReasoningReplayModeOff, r.modeForUser(context.Background(), 428))
	require.EqualValues(t, 2, reads.Load())
	r.snapshot.Store(&nativeReasoningRuntimeSnapshot{until: time.Now().Add(time.Minute), config: NativeReasoningReplayRuntimeConfig{Mode: NativeReasoningReplayModeReplay, UserIDs: []int64{428}, ExpiresAt: time.Now().Add(-time.Second)}})
	require.Equal(t, NativeReasoningReplayModeOff, r.modeForUser(context.Background(), 428))
}

func TestNativeReasoningReplayRuntimeEmptyAllowlistAndCipherDomain(t *testing.T) {
	r := &nativeReasoningRuntime{engine: NewNativeReasoningReplay(nil, nil, DefaultNativeReasoningReplayLimits()), reader: nativeReplayPolicyReaderFunc(func(context.Context) (NativeReasoningReplayRuntimeConfig, error) {
		return NativeReasoningReplayRuntimeConfig{Mode: NativeReasoningReplayModeReplay}, nil
	})}
	require.Equal(t, NativeReasoningReplayModeOff, r.modeForUser(context.Background(), 428))
	cfg := &config.Config{}
	cfg.JWT.Secret = "synthetic-native-replay-key"
	cipher := newNativeReasoningReplayCipher(cfg)
	encrypted, err := cipher.Encrypt("synthetic-output")
	require.NoError(t, err)
	plain, err := cipher.Decrypt(encrypted)
	require.NoError(t, err)
	require.Equal(t, "synthetic-output", plain)
	_, err = newLiveAttestationCipher(cfg).Decrypt(encrypted)
	require.Error(t, err)
	require.Nil(t, newNativeReasoningReplayCipher(&config.Config{}))
	require.Equal(t, DefaultNativeReasoningReplayLimits(), nativeReasoningLimitsFromConfig(&config.Config{}))
}
