package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContentModerationRemoteHeartbeatIsTransportOnlyAndExposed(t *testing.T) {
	var headCalls atomic.Int32
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			headCalls.Add(1)
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPost:
			postCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	channel := contentModerationDeepSeekRuntimeTestChannel("heartbeat", server.URL, 0)
	apiKey := channel.APIKey
	path := filepath.Join(t.TempDir(), "heartbeat-keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))
	envelope, err := cipher.EncryptDeepSeekAPIKey(channel.ID, channel.APIKey)
	require.NoError(t, err)
	channel.APIKey = ""
	channel.APIKeyEnvelope = envelope
	cfg := contentModerationHealthTestConfig(channel)
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(raw),
		SettingKeyRiskControlEnabled:      "true",
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.setContentModerationCredentialCipherForTest(cipher)
	loaded, loadErr := svc.loadConfig(context.Background())
	require.NoError(t, loadErr)
	require.Len(t, loaded.DeepSeekChannels, 1)
	require.True(t, loaded.DeepSeekChannels[0].Enabled)
	require.Equal(t, ContentModerationRemoteProviderDeepSeek, contentModerationRemoteProvider(loaded.DeepSeekChannels[0]))
	require.Equal(t, apiKey, loaded.DeepSeekChannels[0].APIKey)

	svc.runRemoteHeartbeat(context.Background())
	require.Equal(t, int32(1), headCalls.Load())
	require.Zero(t, postCalls.Load(), "transport heartbeat must never invoke a model")

	view, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.Len(t, view.RemoteReviewers, 1)
	require.Equal(t, "reachable", view.RemoteReviewers[0].HeartbeatStatus)
	require.Equal(t, http.StatusNotFound, view.RemoteReviewers[0].HeartbeatHTTPStatus)
	require.NotNil(t, view.RemoteReviewers[0].LastHeartbeatAt)

	status, err := svc.GetStatus(context.Background())
	require.NoError(t, err)
	require.Len(t, status.RemoteHeartbeats, 1)
	require.Equal(t, "heartbeat", status.RemoteHeartbeats[0].ChannelID)
	require.Equal(t, "reachable", status.RemoteHeartbeats[0].Status)
	require.Equal(t, http.StatusNotFound, status.RemoteHeartbeats[0].HTTPStatus)
}

func TestContentModerationLegacyChannelTestUsesHeadOnly(t *testing.T) {
	var headCalls atomic.Int32
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			headCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			postCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	channel := contentModerationDeepSeekRuntimeTestChannel("legacy-head", server.URL, 0)
	apiKey := channel.APIKey
	path := filepath.Join(t.TempDir(), "legacy-head-keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))
	envelope, err := cipher.EncryptDeepSeekAPIKey(channel.ID, channel.APIKey)
	require.NoError(t, err)
	channel.APIKey = ""
	channel.APIKeyEnvelope = envelope
	raw, err := json.Marshal(contentModerationHealthTestConfig(channel))
	require.NoError(t, err)
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(raw),
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.setContentModerationCredentialCipherForTest(cipher)

	result, err := svc.TestDeepSeekChannel(context.Background(), channel.ID)
	require.NoError(t, err)
	require.True(t, result.Reachable)
	require.Empty(t, result.TestType)
	require.Equal(t, int32(1), headCalls.Load())
	require.Zero(t, postCalls.Load(), "legacy /test must remain a transport-only HEAD request")
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), apiKey)
}

func TestContentModerationManualAPIUsabilityTestUsesRealReviewAndReturnsSafeMetadata(t *testing.T) {
	var headCalls atomic.Int32
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			headCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		require.Equal(t, http.MethodPost, r.Method)
		postCalls.Add(1)
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"disposition":"allow","confidence":0.95,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer server.Close()

	const apiKey = "sk-health-test-secret"
	path := filepath.Join(t.TempDir(), "keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))
	channel := contentModerationDeepSeekRuntimeTestChannel("api-usability", server.URL, 0)
	channel.APIKey = ""
	envelope, err := cipher.EncryptDeepSeekAPIKey(channel.ID, apiKey)
	require.NoError(t, err)
	channel.APIKeyEnvelope = envelope
	cfg := contentModerationHealthTestConfig(channel)
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(raw),
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.setContentModerationCredentialCipherForTest(cipher)

	result, err := svc.TestContentModerationChannelAPI(context.Background(), channel.ID)
	require.NoError(t, err)
	require.True(t, result.Reachable)
	require.True(t, result.HealthValid)
	require.Equal(t, "api_usability", result.TestType)
	require.Equal(t, ContentModerationRemoteProviderDeepSeek, result.Provider)
	require.Equal(t, "safe", result.Verdict)
	require.Equal(t, "safe", result.Category)
	require.InDelta(t, 0.05, result.Confidence, 0.0001)
	require.Equal(t, http.StatusOK, result.HTTPStatus)
	require.Zero(t, headCalls.Load())
	require.Equal(t, int32(1), postCalls.Load())

	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), apiKey)
	require.NotContains(t, string(encoded), "Sub2API content moderation reviewer health check")
}

func TestContentModerationStartRunsAPIUsabilityOnce(t *testing.T) {
	var headCalls atomic.Int32
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			headCalls.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		require.Equal(t, http.MethodPost, r.Method)
		postCalls.Add(1)
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"disposition":"allow","confidence":0.05,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))
	channel := contentModerationDeepSeekRuntimeTestChannel("startup-once", server.URL, 0)
	envelope, err := cipher.EncryptDeepSeekAPIKey(channel.ID, channel.APIKey)
	require.NoError(t, err)
	channel.APIKey = ""
	channel.APIKeyEnvelope = envelope
	cfg := contentModerationHealthTestConfig(channel)
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(raw),
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.setContentModerationCredentialCipherForTest(cipher)

	svc.Start()
	svc.Start()
	t.Cleanup(svc.CloseContentModerationRuntime)
	require.Eventually(t, svc.startupAPIUsabilityTested.Load, time.Second, 10*time.Millisecond)
	require.Equal(t, int32(1), postCalls.Load())
	require.Eventually(t, func() bool { return headCalls.Load() == 1 }, time.Second, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(1), postCalls.Load(), "calling Start twice must not repeat the paid test")
	require.Equal(t, int32(1), headCalls.Load(), "calling Start twice must not start a second heartbeat worker")

	status, err := svc.GetStatus(context.Background())
	require.NoError(t, err)
	require.True(t, status.StartupAPIUsabilityTested)
	require.Equal(t, 1, status.StartupAPIUsabilityConfigured)
	require.Equal(t, 1, status.StartupAPIUsabilitySucceeded)
	require.NotNil(t, status.StartupAPIUsabilityCheckedAt)
}

func TestContentModerationReadinessRecoveryRetriesOneChannelAndStopsWhenReady(t *testing.T) {
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		require.Equal(t, http.MethodPost, r.Method)
		if postCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"disposition":"allow","confidence":0.05,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "recovery-keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))
	channel := contentModerationDeepSeekRuntimeTestChannel("readiness-recovery", server.URL, 0)
	envelope, err := cipher.EncryptDeepSeekAPIKey(channel.ID, channel.APIKey)
	require.NoError(t, err)
	channel.APIKey = ""
	channel.APIKeyEnvelope = envelope
	raw, err := json.Marshal(contentModerationHealthTestConfig(channel))
	require.NoError(t, err)
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(raw),
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.setContentModerationCredentialCipherForTest(cipher)

	svc.runRemoteReadinessRecovery(context.Background())
	require.Equal(t, int32(1), postCalls.Load())
	loaded, err := svc.loadConfig(context.Background())
	require.NoError(t, err)
	ready, reason := svc.contentModerationSecondLayerEnforceReadiness(loaded, time.Now())
	require.False(t, ready)
	require.NotEmpty(t, reason)

	svc.runRemoteReadinessRecovery(context.Background())
	require.Equal(t, int32(2), postCalls.Load())
	loaded, err = svc.loadConfig(context.Background())
	require.NoError(t, err)
	ready, reason = svc.contentModerationSecondLayerEnforceReadiness(loaded, time.Now())
	require.True(t, ready, reason)

	svc.runRemoteReadinessRecovery(context.Background())
	require.Equal(t, int32(2), postCalls.Load(), "ready pools must not receive periodic paid probes")
}

func TestContentModerationStartAutomaticallyRecoversReadiness(t *testing.T) {
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		require.Equal(t, http.MethodPost, r.Method)
		if postCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"disposition":"allow","confidence":0.05,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "recovery-worker-keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))
	channel := contentModerationDeepSeekRuntimeTestChannel("recovery-worker", server.URL, 0)
	envelope, err := cipher.EncryptDeepSeekAPIKey(channel.ID, channel.APIKey)
	require.NoError(t, err)
	channel.APIKey = ""
	channel.APIKeyEnvelope = envelope
	raw, err := json.Marshal(contentModerationHealthTestConfig(channel))
	require.NoError(t, err)
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(raw),
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.setContentModerationCredentialCipherForTest(cipher)
	svc.remoteReadinessRetryInitial = 10 * time.Millisecond
	svc.remoteReadinessRetryInterval = 10 * time.Millisecond
	t.Cleanup(svc.CloseContentModerationRuntime)

	svc.Start()
	require.Eventually(t, func() bool {
		loaded, err := svc.loadConfig(context.Background())
		if err != nil {
			return false
		}
		ready, _ := svc.contentModerationSecondLayerEnforceReadiness(loaded, time.Now())
		return ready
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, int32(2), postCalls.Load())
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, int32(2), postCalls.Load(), "automatic recovery must stop paid probes once ready")
}

func TestContentModerationStartupAPIUsabilityTestsProvidersConcurrently(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	serverFor := func(response string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			started <- struct{}{}
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(response))
		}))
	}
	deepSeek := serverFor(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"disposition\":\"allow\",\"confidence\":0.05,\"category\":\"safe\",\"reason\":\"\"}"}}]}`)
	defer deepSeek.Close()
	qwen := serverFor(`{"status":"completed","output_text":"{\"disposition\":\"allow\",\"confidence\":0.05,\"category\":\"safe\",\"reason\":\"\"}"}`)
	defer qwen.Close()

	path := filepath.Join(t.TempDir(), "parallel-keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))
	channels := []ContentModerationDeepSeekChannel{
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "parallel-deepseek", deepSeek.URL, 0),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "parallel-qwen", qwen.URL, 1),
	}
	for i := range channels {
		envelope, err := cipher.EncryptDeepSeekAPIKey(channels[i].ID, channels[i].APIKey)
		require.NoError(t, err)
		channels[i].APIKey = ""
		channels[i].APIKeyEnvelope = envelope
	}
	raw, err := json.Marshal(contentModerationRemotePoolTestConfig(channels...))
	require.NoError(t, err)
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(raw),
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.setContentModerationCredentialCipherForTest(cipher)

	done := make(chan struct{})
	go func() {
		svc.runStartupAPIUsabilityTests(context.Background())
		close(done)
	}()
	for range channels {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			require.FailNow(t, "startup provider tests were not concurrent")
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "startup provider tests did not finish")
	}
	require.Equal(t, int64(2), svc.startupAPIUsabilityConfigured.Load())
	require.Equal(t, int64(2), svc.startupAPIUsabilitySucceeded.Load())
}

func TestContentModerationCloseCancelsBackgroundHealthWorkers(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	var requestStartedOnce atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if requestStartedOnce.CompareAndSwap(false, true) {
			close(requestStarted)
		}
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "cancel-keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	cipher := NewContentModerationCredentialCipher(NewContentModerationArchiveKeyRingFile(path))
	channel := contentModerationDeepSeekRuntimeTestChannel("cancel-startup", server.URL, 0)
	channel.TimeoutMS = 5000
	envelope, err := cipher.EncryptDeepSeekAPIKey(channel.ID, channel.APIKey)
	require.NoError(t, err)
	channel.APIKey = ""
	channel.APIKeyEnvelope = envelope
	raw, err := json.Marshal(contentModerationHealthTestConfig(channel))
	require.NoError(t, err)
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: string(raw),
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.setContentModerationCredentialCipherForTest(cipher)

	svc.Start()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "startup API test did not begin")
	}
	closed := make(chan struct{})
	go func() {
		svc.CloseContentModerationRuntime()
		close(closed)
	}()
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		require.FailNow(t, "startup API request was not canceled")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		require.FailNow(t, "moderation health workers did not stop")
	}
}

func contentModerationHealthTestConfig(channel ContentModerationDeepSeekChannel) *ContentModerationConfig {
	cfg := defaultContentModerationConfig()
	cfg.DeepSeekEnabled = true
	cfg.RemoteReviewersEnabled = true
	cfg.RemoteReviewersVersion = 1
	cfg.RemoteConsensusRequired = contentModerationRemoteConsensusVotes
	cfg.DeepSeekChannels = []ContentModerationDeepSeekChannel{channel}
	cfg.normalize()
	return cfg
}
