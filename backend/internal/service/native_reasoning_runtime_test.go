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
