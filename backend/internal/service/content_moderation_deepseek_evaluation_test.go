package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationDeepSeekEvaluatorReusesProductionContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, map[string]any{"type": "disabled"}, payload["thinking"])
		require.Equal(t, map[string]any{"type": "json_object"}, payload["response_format"])
		require.NotContains(t, payload, "reasoning_effort")
		require.Equal(t, "Bearer sk-evaluation-fixture", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"{\"confidence\":0.91,\"category\":\"cyber_abuse\",\"reason\":\"明确攻击意图\"}"}}]}`))
	}))
	defer server.Close()

	evaluator, err := NewContentModerationDeepSeekEvaluator([]ContentModerationDeepSeekChannel{{
		ID: "official-evaluation", Name: "official evaluation", BaseURL: server.URL,
		Model: DefaultContentModerationDeepSeekModel, Enabled: true, TimeoutMS: 1000,
		APIKey: "sk-evaluation-fixture",
	}}, 2000)
	require.NoError(t, err)

	result, err := evaluator.Evaluate(context.Background(), ContentModerationDeepSeekEvaluationInput{
		Text: "明确请求攻击未授权目标", ContextClass: ContentModerationContextUser, Role: "user", Kind: "text",
	})
	require.NoError(t, err)
	require.True(t, result.Flagged)
	require.Equal(t, 0.91, result.Confidence)
	require.Equal(t, "cyber_abuse", result.Category)
	digest := sha256.Sum256([]byte("明确攻击意图"))
	require.Equal(t, hex.EncodeToString(digest[:]), result.ReasonHash)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "明确攻击意图")
	require.Equal(t, ContentModerationDeepSeekPromptVersion, result.PromptVersion)
	require.Len(t, result.Attempts, 1)
	require.Equal(t, "success", result.Attempts[0].Outcome)
}

func TestContentModerationDeepSeekEvaluatorReturnsParentCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	defer server.Close()

	evaluator, err := NewContentModerationDeepSeekEvaluator([]ContentModerationDeepSeekChannel{{
		ID: "cancel-evaluation", Name: "cancel evaluation", BaseURL: server.URL,
		Model: DefaultContentModerationDeepSeekModel, Enabled: true, TimeoutMS: 3000,
		APIKey: "sk-cancel-fixture",
	}}, 10000)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, evaluateErr := evaluator.Evaluate(ctx, ContentModerationDeepSeekEvaluationInput{
			Text: "synthetic cancellation input", ContextClass: ContentModerationContextUser,
			Role: "user", Kind: "text",
		})
		result <- evaluateErr
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	close(release)
}
