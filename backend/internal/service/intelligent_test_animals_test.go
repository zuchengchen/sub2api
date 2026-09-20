package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIntelligentTestAnimalsAreUniqueAndNumerous(t *testing.T) {
	t.Parallel()
	require.GreaterOrEqual(t, len(intelligentTestAnimals), 100)
	seen := map[string]bool{}
	for _, animal := range intelligentTestAnimals {
		require.NotEmpty(t, animal)
		require.False(t, seen[animal], "duplicate animal %q", animal)
		seen[animal] = true
	}
}

func TestPickIntelligentTestAnimalAvoidsLastChoice(t *testing.T) {
	t.Parallel()
	first := pickIntelligentTestAnimal("")
	require.Contains(t, intelligentTestAnimals, first)
	different := 0
	for i := 0; i < 40; i++ {
		next := pickIntelligentTestAnimal(first)
		require.Contains(t, intelligentTestAnimals, next)
		if next != first {
			different++
		}
	}
	require.Greater(t, different, 20)
}

func TestIntelligentAnimalHTMLPromptInsertsName(t *testing.T) {
	t.Parallel()
	prompt := intelligentAnimalHTMLPrompt("火烈鸟")
	require.Contains(t, prompt, "绘制一个火烈鸟骑自行车")
	require.Contains(t, prompt, "不要依赖我本地的AGENTS.md")
}

func TestPelicanSlotKeyTruncatesToTenMinutes(t *testing.T) {
	t.Parallel()
	now, err := time.Parse(time.RFC3339, "2026-09-20T03:17:44Z")
	require.NoError(t, err)
	require.Equal(t, "pelican-slot-202609200310", pelicanSlotKey(now))
}
