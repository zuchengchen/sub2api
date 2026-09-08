package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestWithOpenAIAccountTestDisplayNames(t *testing.T) {
	got := withOpenAIAccountTestDisplayNames([]openai.Model{
		{ID: "gpt-5.6-sol"},
		{ID: "gpt-reserve", DisplayName: "  "},
		{ID: "gpt-5.6-terra", DisplayName: "GPT-5.6-Terra"},
	})
	require.Equal(t, "GPT-5.6 Sol", got[0].DisplayName)
	require.Equal(t, "model", got[0].Type)
	require.Equal(t, "gpt-reserve", got[1].DisplayName)
	require.Equal(t, "model", got[1].Type)
	require.Equal(t, "GPT-5.6-Terra", got[2].DisplayName)
}
