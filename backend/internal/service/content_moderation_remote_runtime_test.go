package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func contentModerationRemotePoolTestInput(t *testing.T) contentModerationSecondLayerInput {
	t.Helper()
	return contentModerationDeepSeekRuntimeTestInput(t, "审核这段普通测试文本")
}

func contentModerationRemotePoolTestChannel(provider, id, baseURL string, order int) ContentModerationDeepSeekChannel {
	model := DefaultContentModerationDeepSeekModel
	switch provider {
	case ContentModerationRemoteProviderQwen:
		model = "qwen3.7-flash"
	case ContentModerationRemoteProviderGLM:
		model = "glm-4.7-flashx"
	case ContentModerationRemoteProviderMiMo:
		model = "mimo-v2.5"
	}
	return ContentModerationDeepSeekChannel{
		ID: id, Name: id, Provider: provider, BaseURL: baseURL, Model: model,
		Enabled: true, Order: order, TimeoutMS: 1000, APIKey: "test-key-" + id,
	}
}

func contentModerationRemotePoolTestConfig(channels ...ContentModerationDeepSeekChannel) *ContentModerationConfig {
	return &ContentModerationConfig{
		DeepSeekEnabled: true, RemoteReviewersEnabled: true, RemoteReviewersVersion: 1,
		RemoteConsensusRequired: 2, DeepSeekThreshold: DefaultContentModerationDeepSeekThreshold,
		DeepSeekTotalTimeoutMS: 3000, DeepSeekChannels: channels,
	}
}

func contentModerationRemotePoolWriteResult(t *testing.T, w http.ResponseWriter, confidence float64, category, reason string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message": map[string]any{
				"content": mustMarshalContentModerationRemoteDecision(t, confidence, category, reason),
			},
		}},
	})
}

func mustMarshalContentModerationRemoteDecision(t *testing.T, confidence float64, category, reason string) string {
	t.Helper()
	disposition := ContentModerationReviewDispositionViolation
	if category == "safe" {
		disposition = ContentModerationReviewDispositionAllow
	} else if category == ContentModerationRestrictedCategory {
		disposition = ContentModerationReviewDispositionRestricted
	}
	raw, err := json.Marshal(map[string]any{
		"disposition": disposition, "confidence": confidence, "category": category, "reason": reason,
	})
	require.NoError(t, err)
	return string(raw)
}

func contentModerationRemotePoolWriteResponsesResult(t *testing.T, w http.ResponseWriter, confidence float64, category, reason string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "completed",
		"output_text": mustMarshalContentModerationRemoteDecision(t, confidence, category, reason),
	})
}

func TestContentModerationRemotePoolRequiresTwoViolationVotes(t *testing.T) {
	var deepSeekHits, qwenHits atomic.Int32
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deepSeekHits.Add(1)
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		contentModerationRemotePoolWriteResult(t, w, 0.93, "cyber_abuse", "risk")
	}))
	defer deepSeek.Close()
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qwenHits.Add(1)
		require.Equal(t, "/v1/responses", r.URL.Path)
		contentModerationRemotePoolWriteResponsesResult(t, w, 0.91, "cyber_abuse", "risk")
	}))
	defer qwen.Close()

	cfg := contentModerationRemotePoolTestConfig(
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", deepSeek.URL, 0),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "qwen", qwen.URL+"/v1", 1),
	)
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	result, attempted, err := svc.scanContentModerationDeepSeek(context.Background(), cfg, contentModerationRemotePoolTestInput(t))
	require.NoError(t, err)
	require.True(t, attempted)
	require.True(t, result.Blocked)
	require.Equal(t, "confirmed_violation", result.ConsensusStatus)
	require.Equal(t, 2, result.RemoteVotes)
	require.Len(t, result.ReviewAttempts, 2)
	require.Equal(t, "deepseek", result.ReviewAttempts[0].Provider)
	require.Equal(t, "primary", result.ReviewAttempts[0].Role)
	require.Equal(t, "alibaba_qwen", result.ReviewAttempts[1].Provider)
	require.Equal(t, "confirmation", result.ReviewAttempts[1].Role)
	require.Equal(t, int32(1), deepSeekHits.Load())
	require.Equal(t, int32(1), qwenHits.Load())
}

func TestContentModerationRemotePoolConfirmsRestrictedWithoutViolation(t *testing.T) {
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentModerationRemotePoolWriteResult(t, w, 0.95, ContentModerationRestrictedCategory, "restricted")
	}))
	defer deepSeek.Close()
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentModerationRemotePoolWriteResponsesResult(t, w, 0.92, ContentModerationRestrictedCategory, "restricted")
	}))
	defer qwen.Close()

	cfg := contentModerationRemotePoolTestConfig(
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", deepSeek.URL, 0),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "qwen", qwen.URL+"/v1", 1),
	)
	cfg.DeepSeekThreshold = 0.99
	result, attempted, err := (&ContentModerationService{}).scanContentModerationDeepSeek(
		context.Background(), cfg, contentModerationRemotePoolTestInput(t),
	)
	require.NoError(t, err)
	require.True(t, attempted)
	require.True(t, result.Blocked)
	require.Equal(t, ContentModerationReviewDispositionRestricted, result.Disposition)
	require.Equal(t, ContentModerationRestrictedCategory, result.Category)
	require.InDelta(t, 0.92, result.Confidence, 0.0001)
	require.Equal(t, "confirmed_restricted", result.ConsensusStatus)
	require.False(t, result.ReviewerMismatch)
	require.Len(t, result.ReviewAttempts, 2)
	require.Equal(t, "restricted", result.ReviewAttempts[0].Verdict)
	require.Equal(t, "restricted", result.ReviewAttempts[1].Verdict)
}

func TestContentModerationRemotePoolDisagreementBlocksWithoutViolation(t *testing.T) {
	var qwenHits atomic.Int32
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentModerationRemotePoolWriteResult(t, w, 0.92, "weapons", "risk")
	}))
	defer deepSeek.Close()
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		qwenHits.Add(1)
		contentModerationRemotePoolWriteResponsesResult(t, w, 0.08, "safe", "")
	}))
	defer qwen.Close()
	cfg := contentModerationRemotePoolTestConfig(
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", deepSeek.URL, 0),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "qwen", qwen.URL+"/v1", 1),
	)
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	result, attempted, err := svc.scanContentModerationDeepSeek(context.Background(), cfg, contentModerationRemotePoolTestInput(t))
	require.NoError(t, err)
	require.True(t, attempted)
	require.True(t, result.Blocked)
	require.Equal(t, ContentModerationReviewDispositionRestricted, result.Disposition)
	require.Equal(t, ContentModerationRestrictedCategory, result.Category)
	require.InDelta(t, 0.92, result.Confidence, 0.0001)
	require.True(t, result.ReviewerMismatch)
	require.Equal(t, "disagreement_restricted", result.ConsensusStatus)
	require.Equal(t, 2, result.RemoteVotes)
	require.Equal(t, int32(1), qwenHits.Load())
}

func TestContentModerationRemotePoolSafeThenRestrictedStillBlocksWithoutViolation(t *testing.T) {
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentModerationRemotePoolWriteResult(t, w, 0.05, "safe", "")
	}))
	defer deepSeek.Close()
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentModerationRemotePoolWriteResponsesResult(t, w, 0.94, ContentModerationRestrictedCategory, "restricted")
	}))
	defer qwen.Close()

	cfg := contentModerationRemotePoolTestConfig(
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", deepSeek.URL, 0),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "qwen", qwen.URL+"/v1", 1),
	)
	result, attempted, err := (&ContentModerationService{}).scanContentModerationDeepSeek(
		context.Background(), cfg, contentModerationRemotePoolTestInput(t),
	)
	require.NoError(t, err)
	require.True(t, attempted)
	require.True(t, result.Blocked)
	require.Equal(t, ContentModerationReviewDispositionRestricted, result.Disposition)
	require.Equal(t, ContentModerationRestrictedCategory, result.Category)
	require.InDelta(t, 0.94, result.Confidence, 0.0001)
	require.True(t, result.ReviewerMismatch)
	require.Equal(t, "disagreement_restricted", result.ConsensusStatus)
}

func TestContentModerationRemotePoolFailsOverWhenFirstProviderUnavailable(t *testing.T) {
	var qwenHits atomic.Int32
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer deepSeek.Close()
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		qwenHits.Add(1)
		contentModerationRemotePoolWriteResponsesResult(t, w, 0.07, "safe", "")
	}))
	defer qwen.Close()
	cfg := contentModerationRemotePoolTestConfig(
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", deepSeek.URL, 0),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "qwen", qwen.URL+"/v1", 1),
	)
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	result, attempted, err := svc.scanContentModerationDeepSeek(context.Background(), cfg, contentModerationRemotePoolTestInput(t))
	require.NoError(t, err)
	require.True(t, attempted)
	require.False(t, result.Blocked)
	require.Equal(t, "primary_safe", result.ConsensusStatus)
	require.GreaterOrEqual(t, len(result.ReviewAttempts), 2)
	require.Equal(t, "http_502", result.ReviewAttempts[0].Error)
	require.Equal(t, "alibaba_qwen", result.ReviewAttempts[len(result.ReviewAttempts)-1].Provider)
	require.Equal(t, int32(1), qwenHits.Load())
}

func TestContentModerationRemotePoolContinuesAfterUnavailableProviderForConsensus(t *testing.T) {
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer deepSeek.Close()
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentModerationRemotePoolWriteResponsesResult(t, w, 0.94, "weapons", "risk")
	}))
	defer qwen.Close()
	glm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasSuffix(r.URL.Path, "/chat/completions"))
		contentModerationRemotePoolWriteResult(t, w, 0.95, "weapons", "risk")
	}))
	defer glm.Close()
	cfg := contentModerationRemotePoolTestConfig(
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", deepSeek.URL, 0),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "qwen", qwen.URL+"/v1", 1),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderGLM, "glm", glm.URL+"/api/paas/v4", 2),
	)
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	result, attempted, err := svc.scanContentModerationDeepSeek(context.Background(), cfg, contentModerationRemotePoolTestInput(t))
	require.NoError(t, err)
	require.True(t, attempted)
	require.True(t, result.Blocked)
	require.Equal(t, "confirmed_violation", result.ConsensusStatus)
	require.Equal(t, 2, result.RemoteVotes)
	successProviders := make([]string, 0, 2)
	for _, attempt := range result.ReviewAttempts {
		if attempt.Outcome == "success" {
			successProviders = append(successProviders, attempt.Provider)
		}
	}
	require.Equal(t, []string{"alibaba_qwen", "zhipu_glm"}, successProviders)
}

func TestContentModerationRemotePoolDoesNotBlockOnSingleViolation(t *testing.T) {
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentModerationRemotePoolWriteResult(t, w, 0.96, "cyber_abuse", "risk")
	}))
	defer deepSeek.Close()
	qwen := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer qwen.Close()
	cfg := contentModerationRemotePoolTestConfig(
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", deepSeek.URL, 0),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "qwen", qwen.URL+"/v1", 1),
	)
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	result, attempted, err := svc.scanContentModerationDeepSeek(context.Background(), cfg, contentModerationRemotePoolTestInput(t))
	require.Error(t, err)
	require.True(t, attempted)
	require.False(t, result.Blocked)
	require.Equal(t, "consensus_unavailable", result.ConsensusStatus)
	require.Equal(t, 1, result.RemoteVotes)
}

func TestContentModerationRemotePayloadDisablesReasoningForResponsesProviders(t *testing.T) {
	input := contentModerationRemotePoolTestInput(t)
	channel := contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderMiMo, "mimo", "https://api.xiaomimimo.com/v1", 0)
	payload, err := buildContentModerationRemotePayload(channel, input)
	require.NoError(t, err)
	require.Equal(t, "mimo-v2.5", payload["model"])
	require.Equal(t, false, payload["stream"])
	require.Equal(t, 96, payload["max_output_tokens"])
	require.Equal(t, map[string]string{"effort": "none"}, payload["reasoning"])
	require.NotContains(t, payload, "thinking")
	require.NotContains(t, payload, "reasoning_effort")
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), channel.APIKey)
}

func TestContentModerationRemoteReviewerDefaultsAndLegacyMigration(t *testing.T) {
	cfg, err := parseContentModerationConfig("")
	require.NoError(t, err)
	require.True(t, cfg.RemoteReviewersEnabled)
	require.Equal(t, 1, cfg.RemoteReviewersVersion)
	require.Equal(t, 2, cfg.RemoteConsensusRequired)
	require.Len(t, cfg.DeepSeekChannels, 4)
	providers := make([]string, 0, len(cfg.DeepSeekChannels))
	for _, channel := range cfg.DeepSeekChannels {
		require.True(t, channel.Enabled)
		providers = append(providers, channel.Provider)
	}
	require.Equal(t, []string{"deepseek", "alibaba_qwen", "zhipu_glm", "mimo"}, providers)

	legacy := &ContentModerationConfig{
		DeepSeekEnabled: true,
		DeepSeekChannels: []ContentModerationDeepSeekChannel{{
			ID: "legacy-ds", Name: "Legacy DS", BaseURL: DefaultContentModerationDeepSeekBaseURL,
			Model: DefaultContentModerationDeepSeekModel, Enabled: true, TimeoutMS: 1000, APIKey: "test-legacy-key",
		}},
	}
	legacy.normalize()
	require.Len(t, legacy.DeepSeekChannels, 1)
	require.Equal(t, "legacy-ds", legacy.DeepSeekChannels[0].ID)
	require.Equal(t, "test-legacy-key", legacy.DeepSeekChannels[0].APIKey)
	require.Equal(t, "deepseek", legacy.DeepSeekChannels[0].Provider)

	persisted, err := parseContentModerationConfig(`{
		"deepseek_enabled":true,
		"deepseek_channels":[{
			"id":"legacy-ds","name":"Legacy DS","base_url":"https://api.deepseek.com",
			"model":"deepseek-v4-flash","enabled":true,"order":0,"timeout_ms":1000
		}]
	}`)
	require.NoError(t, err)
	require.Equal(t, 1, persisted.RemoteReviewersVersion)
	require.Len(t, persisted.DeepSeekChannels, 4)
	require.Equal(t, []string{"deepseek", "alibaba_qwen", "zhipu_glm", "mimo"}, []string{
		persisted.DeepSeekChannels[0].Provider,
		persisted.DeepSeekChannels[1].Provider,
		persisted.DeepSeekChannels[2].Provider,
		persisted.DeepSeekChannels[3].Provider,
	})
}

func TestContentModerationUpdateConfigOptsLegacyConfigIntoRemotePool(t *testing.T) {
	repo := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyContentModerationConfig: `{"deepseek_enabled":true,"deepseek_channels":[{"id":"legacy-ds","name":"Legacy DS","base_url":"https://api.deepseek.com","model":"deepseek-v4-flash","enabled":true,"order":0,"timeout_ms":1000}]}`,
	}}
	svc := NewContentModerationService(repo, nil, nil, nil, nil, nil, nil, nil)
	enabled := true
	view, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{
		RemoteReviewersEnabled: &enabled,
	})
	require.NoError(t, err)
	require.Len(t, view.RemoteReviewers, 4)
	require.Equal(t, []string{"deepseek", "alibaba_qwen", "zhipu_glm", "mimo"}, []string{
		view.RemoteReviewers[0].Provider,
		view.RemoteReviewers[1].Provider,
		view.RemoteReviewers[2].Provider,
		view.RemoteReviewers[3].Provider,
	})
}

func TestContentModerationRemoteProviderOrderIsPolicyOwned(t *testing.T) {
	channels := []ContentModerationDeepSeekChannel{
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderMiMo, "mimo", "https://api.xiaomimimo.com/v1", 0),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderGLM, "glm", "https://open.bigmodel.cn/api/paas/v4", 1),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderQwen, "qwen", "https://dashscope.aliyuncs.com/compatible-mode/v1", 2),
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", "https://api.deepseek.com", 3),
	}
	groups := contentModerationRemoteProviderGroups(channels)
	require.Len(t, groups, 4)
	require.Equal(t, []string{"deepseek", "alibaba_qwen", "zhipu_glm", "mimo"}, []string{
		groups[0].provider, groups[1].provider, groups[2].provider, groups[3].provider,
	})
}

func TestContentModerationRemotePoolUsesProviderToggleAsAuthority(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentModerationRemotePoolWriteResult(t, w, 0.05, "safe", "")
	}))
	defer server.Close()
	cfg := contentModerationRemotePoolTestConfig(
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", server.URL, 0),
	)
	cfg.DeepSeekEnabled = false // stale legacy alias must not disable the v1 pool
	result, attempted, err := (&ContentModerationService{}).scanContentModerationDeepSeek(
		context.Background(), cfg, contentModerationRemotePoolTestInput(t),
	)
	require.NoError(t, err)
	require.True(t, attempted)
	require.Equal(t, "primary_safe", result.ConsensusStatus)
}

func TestContentModerationYuFengShadowCannotOverrideRemoteDecision(t *testing.T) {
	deepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contentModerationRemotePoolWriteResult(t, w, 0.04, "safe", "")
	}))
	defer deepSeek.Close()
	yuFeng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pi<explanation>local risk</explanation>"}}]}`))
	}))
	defer yuFeng.Close()
	cfg := contentModerationRemotePoolTestConfig(
		contentModerationRemotePoolTestChannel(ContentModerationRemoteProviderDeepSeek, "ds", deepSeek.URL, 0),
	)
	cfg.SecondLayerEnabled = true
	cfg.YuFengEnabled = true
	cfg.YuFengMode = ContentModerationYuFengModeShadow
	cfg.SecondLayerEndpoints = []ContentModerationEndpoint{{
		ID: "yufeng", Name: "YuFeng", BaseURL: yuFeng.URL, Model: "yufeng",
		Profile: ContentModerationModelProfileYuFengXGuard, PromptVersion: ContentModerationYuFengPromptVersion,
		Enabled: true, TimeoutMS: 1000, InputLimit: 4000,
	}}
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	result, attempted, err := svc.scanUnifiedSecondLayerPrepared(context.Background(), cfg, contentModerationRemotePoolTestInput(t))
	require.NoError(t, err)
	require.True(t, attempted)
	require.False(t, result.Blocked)
	require.Equal(t, "primary_safe", result.ConsensusStatus)
	require.Len(t, result.ReviewAttempts, 2)
	require.Equal(t, "yufeng", result.ReviewAttempts[1].Provider)
	require.Equal(t, "local_shadow", result.ReviewAttempts[1].Role)
	require.Equal(t, "violation", result.ReviewAttempts[1].Verdict)
}

func TestContentModerationVersionedYuFengShadowFailureDoesNotBlock(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	cfg := &ContentModerationConfig{
		DeepSeekEnabled: false, RemoteReviewersEnabled: false, RemoteReviewersVersion: 1,
		YuFengEnabled: true, YuFengMode: ContentModerationYuFengModeShadow,
		SecondLayerEnabled: true, SecondLayerEndpoints: []ContentModerationEndpoint{{
			ID: "yufeng", Name: "YuFeng", BaseURL: deadURL, Model: "yufeng",
			Profile: ContentModerationModelProfileYuFengXGuard, Enabled: true,
			TimeoutMS: 100, InputLimit: 4000,
		}},
	}
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	result, attempted, err := svc.scanUnifiedSecondLayerPrepared(
		context.Background(), cfg, contentModerationRemotePoolTestInput(t),
	)
	require.NoError(t, err)
	require.True(t, attempted)
	require.False(t, result.Blocked)
	require.Equal(t, "local_shadow", result.ConsensusStatus)
	require.Len(t, result.ReviewAttempts, 1)
	require.Equal(t, "error", result.ReviewAttempts[0].Outcome)
}
