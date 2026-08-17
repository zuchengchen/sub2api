package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContentModerationDeepSeekConfigDefaultsAndLegacyFieldsAreNotExposed(t *testing.T) {
	cfg := defaultContentModerationConfig()
	require.True(t, cfg.DeepSeekEnabled)
	require.False(t, cfg.YuFengEnabled)
	require.Equal(t, DefaultContentModerationDeepSeekTotalTimeoutMS, cfg.DeepSeekTotalTimeoutMS)
	require.Equal(t, DefaultContentModerationDeepSeekThreshold, cfg.DeepSeekThreshold)
	require.Equal(t, ContentModerationFirstLayerStageShadow, cfg.FirstLayerStage)
	require.Equal(t, ContentModerationSecondLayerStageShadow, cfg.SecondLayerStage)
	require.Equal(t, defaultContentModerationDeepSeekChannels(), cfg.DeepSeekChannels)

	view := (&ContentModerationService{}).configView(cfg)
	require.Equal(t, ContentModerationDeepSeekPromptVersion, view.PolicyVersion)
	require.Len(t, view.DeepSeekChannels, 1)
	require.Equal(t, "deepseek-official", view.DeepSeekChannels[0].ID)
	require.False(t, view.DeepSeekChannels[0].APIKeyConfigured)

	raw, err := json.Marshal(view)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	for _, legacy := range []string{
		"base_url", "model", "proxy_id", "api_key", "api_keys", "api_key_configured",
		"api_key_masked", "api_key_count", "api_key_masks", "api_key_statuses", "timeout_ms",
		"sample_rate", "thresholds", "retry_count", "keyword_blocking_mode",
	} {
		_, exists := fields[legacy]
		require.Falsef(t, exists, "legacy field %s must not be exposed", legacy)
	}
}

func TestContentModerationDeepSeekConfigEncryptsPreservesAndClearsAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	repo := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.ConfigureContentModerationCredentialKeyRing(path)
	secret := "sk-unit-test-deepseek-key"
	channels := []ContentModerationDeepSeekChannelInput{{
		ID: "official", Name: "Official", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash",
		Enabled: true, Order: 0, TimeoutMS: 3000, APIKey: secret,
	}}

	view, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{DeepSeekChannels: &channels})
	require.NoError(t, err)
	require.Len(t, view.DeepSeekChannels, 1)
	require.True(t, view.DeepSeekChannels[0].APIKeyConfigured)
	require.Equal(t, maskSecretTail(secret), view.DeepSeekChannels[0].APIKeyMasked)

	stored := repo.values[SettingKeyContentModerationConfig]
	require.NotContains(t, stored, secret)
	var disk struct {
		Channels []ContentModerationDeepSeekChannel `json:"deepseek_channels"`
	}
	require.NoError(t, json.Unmarshal([]byte(stored), &disk))
	require.Len(t, disk.Channels, 1)
	require.NotNil(t, disk.Channels[0].APIKeyEnvelope)
	require.Empty(t, disk.Channels[0].APIKey)
	firstEnvelope := cloneContentModerationCredentialEnvelope(disk.Channels[0].APIKeyEnvelope)

	reloaded := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	reloaded.ConfigureContentModerationCredentialKeyRing(path)
	view, err = reloaded.GetConfig(context.Background())
	require.NoError(t, err)
	require.True(t, view.DeepSeekChannels[0].APIKeyConfigured)
	require.Equal(t, maskSecretTail(secret), view.DeepSeekChannels[0].APIKeyMasked)

	channels[0].Name = "Primary"
	channels[0].APIKey = ""
	view, err = reloaded.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{DeepSeekChannels: &channels})
	require.NoError(t, err)
	require.Equal(t, "Primary", view.DeepSeekChannels[0].Name)
	require.True(t, view.DeepSeekChannels[0].APIKeyConfigured)
	disk.Channels = nil
	require.NoError(t, json.Unmarshal([]byte(repo.values[SettingKeyContentModerationConfig]), &disk))
	require.Equal(t, firstEnvelope, disk.Channels[0].APIKeyEnvelope)

	channels[0].ClearAPIKey = true
	view, err = reloaded.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{DeepSeekChannels: &channels})
	require.NoError(t, err)
	require.False(t, view.DeepSeekChannels[0].APIKeyConfigured)
	require.Empty(t, view.DeepSeekChannels[0].APIKeyMasked)
	disk.Channels = nil
	require.NoError(t, json.Unmarshal([]byte(repo.values[SettingKeyContentModerationConfig]), &disk))
	require.Nil(t, disk.Channels[0].APIKeyEnvelope)
}

func TestContentModerationDeepSeekConfigRejectsInvalidChannelAndPlaintextWithoutKeyRing(t *testing.T) {
	repo := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	channels := []ContentModerationDeepSeekChannelInput{{
		ID: "valid", Name: "Valid", BaseURL: "https://example.com", Model: "model",
		Enabled: true, Order: 0, TimeoutMS: 3000, APIKey: "sk-unit-test",
	}}
	_, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{DeepSeekChannels: &channels})
	require.Error(t, err)
	require.Contains(t, err.Error(), ErrModerationCredentialKeyUnavailable.Error())
	require.Empty(t, repo.values)

	channels[0].APIKey = ""
	channels[0].ID = "bad channel"
	channels[0].BaseURL = "http://example.com"
	_, err = svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{DeepSeekChannels: &channels})
	require.Error(t, err)
	require.Contains(t, err.Error(), "ID")
	require.Empty(t, repo.values)
}

func TestParseContentModerationConfigIgnoresRetiredModerationSecrets(t *testing.T) {
	cfg, err := parseContentModerationConfig(`{
		"base_url":"https://legacy.invalid","model":"legacy-model","api_key":"legacy-plaintext",
		"api_keys":["legacy-pool"],"timeout_ms":9999,"thresholds":{"sexual":0.1},"retry_count":5,
		"second_layer_endpoints":[{"id":"retired","base_url":"http://127.0.0.1:8080","profile":"unsupported_guard","enabled":true}]
	}`)
	require.NoError(t, err)
	require.Equal(t, defaultContentModerationDeepSeekChannels(), cfg.DeepSeekChannels)
	require.Empty(t, cfg.SecondLayerEndpoints)
}

func TestValidateContentModerationDeepSeekBaseURL(t *testing.T) {
	for _, valid := range []string{"https://api.deepseek.com", "https://example.com/api/v1", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		require.NoError(t, validateContentModerationDeepSeekBaseURL(valid), valid)
	}
	for _, invalid := range []string{"", "ftp://example.com", "http://example.com", "https://user:pass@example.com", "https://example.com?q=secret"} {
		require.Error(t, validateContentModerationDeepSeekBaseURL(invalid), invalid)
	}
}

func TestContentModerationSecondLayerEnforceReadinessRequiresRecentSuccessfulReview(t *testing.T) {
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	cfg := defaultContentModerationConfig()
	cfg.DeepSeekChannels[0].APIKey = "sk-unit-test-health"
	backup := cfg.DeepSeekChannels[0]
	backup.ID = "deepseek-backup"
	backup.Name = "DeepSeek Backup"
	backup.BaseURL = "https://backup.example.com"
	backup.Order = 1
	backup.APIKey = "sk-unit-test-backup"
	cfg.DeepSeekChannels = append(cfg.DeepSeekChannels, backup)
	now := time.Now()

	ready, reason := svc.contentModerationSecondLayerEnforceReadiness(cfg, now)
	require.False(t, ready)
	require.Contains(t, reason, "真实审核")

	state := svc.deepSeekChannelState(cfg.DeepSeekChannels[1])
	state.markReviewHealthy(now, contentModerationDeepSeekChannelDigest(cfg.DeepSeekChannels[1]))
	ready, reason = svc.contentModerationSecondLayerEnforceReadiness(cfg, now)
	require.True(t, ready)
	require.Empty(t, reason)
	view := svc.configView(cfg)
	require.Equal(t, "untested", view.DeepSeekChannels[0].HealthStatus)
	require.Equal(t, "reachable", view.DeepSeekChannels[1].HealthStatus)
	ready, reason = svc.contentModerationSecondLayerEnforceReadiness(
		cfg, now.Add(contentModerationDeepSeekHealthTTL+time.Second),
	)
	require.False(t, ready)
	require.Contains(t, reason, "真实审核")

	changed := cloneContentModerationConfig(cfg)
	changed.DeepSeekChannels[1].Model = "different-model"
	ready, reason = svc.contentModerationSecondLayerEnforceReadiness(changed, now)
	require.False(t, ready)
	require.Contains(t, reason, "真实审核")
	svc.deepSeekChannelState(changed.DeepSeekChannels[1]).markReviewHealthy(
		now, contentModerationDeepSeekChannelDigest(changed.DeepSeekChannels[1]),
	)
	ready, reason = svc.contentModerationSecondLayerEnforceReadiness(changed, now)
	require.True(t, ready)
	require.Empty(t, reason)

	changed.DeepSeekChannels[1].BaseURL = "https://changed.example.com"
	ready, reason = svc.contentModerationSecondLayerEnforceReadiness(changed, now)
	require.False(t, ready)
	require.Contains(t, reason, "真实审核")
}

func TestContentModerationUpdateConfigEnforceRequiresSuccessfulReviewAndDoesNotPersistFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring.json")
	writeModerationArchiveTestKeyRing(t, path, "k1", map[string][]byte{
		"k1": []byte("0123456789abcdef0123456789abcdef"),
	})
	repo := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	svc.ConfigureContentModerationCredentialKeyRing(path)
	stage := ContentModerationSecondLayerStageEnforce
	deadServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := deadServer.URL
	deadServer.Close()
	channels := []ContentModerationDeepSeekChannelInput{{
		ID: "official", Name: "Official", BaseURL: deadURL, Model: DefaultContentModerationDeepSeekModel,
		Enabled: true, Order: 0, TimeoutMS: 100, APIKey: "sk-unit-test-enforce",
	}}

	_, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{
		DeepSeekChannels: &channels,
		SecondLayerStage: &stage,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "CONTENT_MODERATION_ENFORCE_NOT_READY")
	_, persisted := repo.values[SettingKeyContentModerationConfig]
	require.False(t, persisted, "failed Enforce readiness must not persist configuration")

	method := make(chan string, 1)
	reachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method <- r.Method
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"confidence":0.05,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer reachable.Close()
	channels[0].BaseURL = reachable.URL
	view, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{
		DeepSeekChannels: &channels,
		SecondLayerStage: &stage,
	})
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, <-method)
	require.Equal(t, ContentModerationSecondLayerStageEnforce, view.SecondLayerStage)

	stored := repo.values[SettingKeyContentModerationConfig]
	require.NotContains(t, stored, channels[0].APIKey)
	var saved ContentModerationConfig
	require.NoError(t, json.Unmarshal([]byte(stored), &saved))
	require.Equal(t, ContentModerationSecondLayerStageEnforce, saved.SecondLayerStage)
	require.Len(t, saved.DeepSeekChannels, 1)
	require.NotNil(t, saved.DeepSeekChannels[0].APIKeyEnvelope)
}

func TestContentModerationSecondLayerEnforceReadinessRequiresRecentYuFengSuccess(t *testing.T) {
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	cfg := defaultContentModerationConfig()
	cfg.DeepSeekEnabled = false
	cfg.YuFengEnabled = true
	cfg.SecondLayerEndpoints = []ContentModerationEndpoint{{
		ID: "yufeng-health", BaseURL: "http://127.0.0.1:8080", Model: "yufeng",
		Profile: ContentModerationModelProfileYuFengXGuard, Enabled: true, TimeoutMS: 1000, InputLimit: 4000,
	}}
	cfg.normalize()
	now := time.Now()

	ready, reason := svc.contentModerationSecondLayerEnforceReadiness(cfg, now)
	require.False(t, ready)
	require.Contains(t, reason, "没有成功完成真实审核")

	svc.markYuFengEndpointHealthy(cfg.SecondLayerEndpoints[0], now)
	ready, reason = svc.contentModerationSecondLayerEnforceReadiness(cfg, now)
	require.True(t, ready)
	require.Empty(t, reason)

	changed := cloneContentModerationConfig(cfg)
	changed.SecondLayerEndpoints[0].Model = "yufeng-new"
	ready, reason = svc.contentModerationSecondLayerEnforceReadiness(changed, now)
	require.False(t, ready)
	require.Contains(t, reason, "没有成功完成真实审核")

	svc.markYuFengEndpointHealthy(changed.SecondLayerEndpoints[0], now)
	ready, _ = svc.contentModerationSecondLayerEnforceReadiness(changed, now.Add(contentModerationYuFengHealthTTL+time.Second))
	require.False(t, ready)
}
