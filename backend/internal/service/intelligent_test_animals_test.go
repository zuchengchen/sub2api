package service

import (
	"strings"
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

func TestAdminPelicanRunPicksRandomAnimal(t *testing.T) {
	t.Parallel()
	svc := &IntelligentTestService{}
	req := svc.withAdminPelicanAnimal(IntelligentTestEnqueue{TestTypes: []string{"pelican", "candy"}})
	prompt := req.Prompts["pelican"]
	require.Contains(t, prompt, "骑自行车的2D动画")
	matched := false
	for _, animal := range intelligentTestAnimals {
		if strings.Contains(prompt, "绘制一个"+animal+"骑自行车") {
			matched = true
			break
		}
	}
	require.True(t, matched, "prompt %q should name a known animal", prompt)
	kept := svc.withAdminPelicanAnimal(IntelligentTestEnqueue{TestTypes: []string{"pelican"}, Prompts: map[string]string{"pelican": "keep"}})
	require.Equal(t, "keep", kept.Prompts["pelican"])
	scheduled := svc.withAdminPelicanAnimal(IntelligentTestEnqueue{TestTypes: []string{"pelican"}, Source: IntelligentTestSourcePelicanSchedule})
	require.Empty(t, scheduled.Prompts)
}

func TestPelicanSlotKeyTruncatesToThirtyMinutes(t *testing.T) {
	t.Parallel()
	now, err := time.Parse(time.RFC3339, "2026-09-20T03:37:44Z")
	require.NoError(t, err)
	require.Equal(t, "pelican-slot-202609200330", pelicanSlotKey(now))
	same, err := time.Parse(time.RFC3339, "2026-09-20T03:39:59Z")
	require.NoError(t, err)
	require.Equal(t, "pelican-slot-202609200330", pelicanSlotKey(same))
	next, err := time.Parse(time.RFC3339, "2026-09-20T04:00:00Z")
	require.NoError(t, err)
	require.Equal(t, "pelican-slot-202609200400", pelicanSlotKey(next))
}

func TestPelicanInBeijingWindowIsEightToMidnight(t *testing.T) {
	t.Parallel()
	parse := func(s string) time.Time {
		t.Helper()
		ts, err := time.Parse(time.RFC3339, s)
		require.NoError(t, err)
		return ts
	}
	require.True(t, pelicanInBeijingWindow(parse("2026-09-20T00:00:00Z")))  // 08:00
	require.True(t, pelicanInBeijingWindow(parse("2026-09-20T15:59:59Z")))  // 23:59
	require.False(t, pelicanInBeijingWindow(parse("2026-09-20T16:00:00Z"))) // 00:00
	require.False(t, pelicanInBeijingWindow(parse("2026-09-19T23:59:59Z"))) // 07:59
}

func TestPelicanNextBoundaryAlignsToThirtyMinutes(t *testing.T) {
	t.Parallel()
	now, err := time.Parse(time.RFC3339, "2026-09-19T23:50:00Z") // 07:50 Beijing
	require.NoError(t, err)
	next := pelicanNextBoundary(now)
	require.Equal(t, "2026-09-20T08:00:00+08:00", next.Format(time.RFC3339))
	require.True(t, pelicanInBeijingWindow(next))
	inside, err := time.Parse(time.RFC3339, "2026-09-20T00:07:00Z") // 08:07 Beijing
	require.NoError(t, err)
	require.Equal(t, "2026-09-20T08:30:00+08:00", pelicanNextBoundary(inside).Format(time.RFC3339))
}
