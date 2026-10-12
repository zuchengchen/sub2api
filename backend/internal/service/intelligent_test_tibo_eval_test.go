package service

import (
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestTiboQuestionMatchesProbePrompt(t *testing.T) {
	t.Parallel()
	body, err := migrations.FS.ReadFile("268_intelligent_test_tibo.sql")
	require.NoError(t, err)
	require.Contains(t, string(body), openAITiboProbePrompt)
}

func TestEvaluateIntelligentTibo(t *testing.T) {
	t.Parallel()
	cfg := IntelligentTestConfig{Evaluator: "tibo_answer", ExpectedAnswer: tiboAnswerTrue}
	got := evaluateIntelligentTibo("True", cfg)
	require.Equal(t, "completed", got.Status)
	require.Equal(t, "correct", got.Detail["answer_verdict"])
	require.Equal(t, "not_required", got.Detail["format_verdict"])
	require.Equal(t, tiboAnswerTrue, got.Detail["actual_answer"])
	require.NotNil(t, got.Score)
	require.Equal(t, 100.0, *got.Score)
	require.Nil(t, got.Detail["reason"])
	require.Nil(t, got.Detail["limitation"])

	got = evaluateIntelligentTibo("**False**", cfg)
	require.Equal(t, "incorrect", got.Detail["answer_verdict"])
	require.Equal(t, tiboAnswerFalse, got.Detail["actual_answer"])
	require.NotNil(t, got.Score)
	require.Equal(t, 0.0, *got.Score)

	got = evaluateIntelligentTibo("I cannot say", cfg)
	require.Equal(t, "undetermined", got.Detail["answer_verdict"])
	require.Equal(t, tiboAnswerOther, got.Detail["actual_answer"])
	require.Nil(t, got.Score)

	got = evaluateIntelligentTibo("   ", cfg)
	require.Equal(t, "undetermined", got.Detail["answer_verdict"])
	require.Equal(t, tiboAnswerNone, got.Detail["actual_answer"])
}

func TestDefaultEvaluatorsIncludeTibo(t *testing.T) {
	t.Parallel()
	evaluators := DefaultIntelligentTestEvaluators()
	require.Contains(t, evaluators, "tibo_answer")
	got := evaluators["tibo_answer"].Evaluate("Yes", IntelligentTestConfig{ExpectedAnswer: "True"})
	require.Equal(t, "correct", got.Detail["answer_verdict"])
}

func TestEvaluateIntelligentSVGOmitsReviewNotes(t *testing.T) {
	t.Parallel()
	got := evaluateIntelligentSVG(`<svg viewBox="0 0 10 10"><circle r="3" cx="5" cy="5"/></svg>`)
	require.Equal(t, "not_evaluated", got.Detail["answer_verdict"])
	require.Equal(t, "compliant", got.Detail["format_verdict"])
	require.Nil(t, got.Detail["reason"])
	require.Nil(t, got.Detail["format_reason"])
	require.Nil(t, got.Detail["limitation"])
	require.NotContains(t, fmt.Sprint(got.Detail), "尚未评估")
	require.NotContains(t, fmt.Sprint(got.Detail), "静态样式")
	require.NotContains(t, fmt.Sprint(got.Detail), "单次测试不足以")
}
