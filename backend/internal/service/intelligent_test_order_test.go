package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSortIntelligentTestSettingsPutsTiboThenCandy(t *testing.T) {
	t.Parallel()
	settings := []IntelligentTestSetting{
		{TestType: "pelican"},
		{TestType: "candy"},
		{TestType: "tibo"},
		{TestType: "alpha"},
	}
	SortIntelligentTestSettings(settings)
	got := make([]string, len(settings))
	for i, s := range settings {
		got[i] = s.TestType
	}
	require.Equal(t, []string{"tibo", "candy", "pelican", "alpha"}, got)
}
