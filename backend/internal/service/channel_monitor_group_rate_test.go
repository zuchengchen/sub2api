//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAttachGroupRatesMatchesNameAndFallsBackToMonitorName(t *testing.T) {
	gptRate := 0.25
	kiroRate := 0.5
	deepseekRate := 1.0
	sweRate := 0.05
	views := []*UserMonitorView{
		{Name: "GPT-6", GroupName: "GPT-PRO"},
		{Name: "GROK-4.5", GroupName: "grok"},
		{Name: "devin2claude", GroupName: ""},
		{Name: "Kiro2Claude", GroupName: "Kiro2Claude"},
		{Name: "DeepSeek", GroupName: "DeepSeek"},
		{Name: "Devin-swe2", GroupName: "Devin-swe2"},
		{Name: "unmatched", GroupName: "missing"},
	}
	attachGroupRates(views, []ChannelMonitorGroupRate{
		{ID: 37, Name: "GPT-PRO", Platform: "openai", SubscriptionType: "standard", RateMultiplier: gptRate},
		{ID: 38, Name: "grok-heavy", Platform: "grok", SubscriptionType: "standard", RateMultiplier: 0.1},
		{ID: 41, Name: "Kiro2Claude", Platform: "anthropic", SubscriptionType: "standard", RateMultiplier: kiroRate},
		{ID: 74, Name: "Devin-swe2", Platform: "openai", SubscriptionType: "standard", RateMultiplier: sweRate},
		{ID: 79, Name: "devin2claude", Platform: "anthropic", SubscriptionType: "standard", RateMultiplier: 1.2},
		{ID: 84, Name: "DeepSeek", Platform: "deepseek", SubscriptionType: "standard", RateMultiplier: deepseekRate},
	})

	require.Equal(t, int64(37), *views[0].GroupID)
	require.Equal(t, gptRate, *views[0].RateMultiplier)
	require.Nil(t, views[1].RateMultiplier)
	require.Equal(t, "devin2claude", views[2].GroupName)
	require.Equal(t, int64(79), *views[2].GroupID)
	require.InDelta(t, 1.2, *views[2].RateMultiplier, 1e-9)
	require.Equal(t, kiroRate, *views[3].RateMultiplier)
	require.Equal(t, "deepseek", views[4].GroupPlatform)
	require.Equal(t, deepseekRate, *views[4].RateMultiplier)
	require.Equal(t, sweRate, *views[5].RateMultiplier)
	require.Nil(t, views[6].GroupID)
	require.Nil(t, views[6].RateMultiplier)
}
