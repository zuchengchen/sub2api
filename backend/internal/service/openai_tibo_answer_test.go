package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyTiboAnswer(t *testing.T) {
	cases := map[string][]string{
		tiboAnswerTrue: {
			"True", "true", "TRUE", "true.", "True.", " True \n", "True!", "True。", "True！",
			"**True**", "*True*", "__True__", "`True`", "```\nTrue\n```", "```text\nTrue\n```",
			`"True"`, "'true'", "“True”", "「True」", "(True)", "[True]", "<b>True</b>",
			"Ｔｒｕｅ", "True ✅", "✅", "✔️", "Yes", "yes.", "是", "是的", "对", "真", "正确",
			"Answer: True", "answer：true", "**Answer:** True", "The answer is True.", "Final answer: True",
			"答案：True", "答案是真", `{"answer": true}`, `{"result":"True"}`,
			"True, Tibo leads Codex at OpenAI.", "True - he leads Codex.", "True\n\nThibault Sottiaux.",
			"是的，我知道", "I know him from Codex.\nTrue", "Tibo is Thibault Sottiaux. True.",
		},
		tiboAnswerFalse: {
			"False", "false", "false.", "**False**", "`False`", "No", "否", "不知道", "假", "❌",
			"Answer: False", "False, I do not know who that is.",
		},
		tiboAnswerNone: {"", "  \n\t"},
		tiboAnswerOther: {
			"True or False", "False. Actually true.", "I cannot answer without searching.",
			"Tibo", "Trueish", "T. Sottiaux leads Codex.", "not true", "Maybe", "Unknown", "Answer",
			"It is true that I am unsure, false otherwise.", "...", "✅❌",
		},
	}
	for want, answers := range cases {
		for _, answer := range answers {
			require.Equal(t, want, classifyTiboAnswer(answer), "answer %q", answer)
		}
	}
}

func TestOpenAITiboHTTPProbeAcceptsDecoratedTrue(t *testing.T) {
	for _, answer := range []string{"true", "True.", "**True**", "Answer: true"} {
		body := "data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, answer)) + "\n\n"
		obs := observeOpenAITiboHTTPProbe([]byte(body))
		require.True(t, obs.completed && obs.trueAnswer && !obs.failed, "answer %q", answer)
	}
	body := "data: " + string(cookieWSCompletion(openAICodexTicketDefaultModel, "false.")) + "\n\n"
	obs := observeOpenAITiboHTTPProbe([]byte(body))
	require.False(t, obs.trueAnswer)
}
