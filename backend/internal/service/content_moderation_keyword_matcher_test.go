package service

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationKeywordMatcherMatchesReferenceBehavior(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		keywords []string
	}{
		{name: "miss", text: "clean prompt", keywords: []string{"blocked", "secret"}},
		{name: "case insensitive", text: "contains SECRET value", keywords: []string{"secret"}},
		{name: "configured order wins", text: "early appears before later", keywords: []string{"later", "early"}},
		{name: "overlap uses configured order", text: "abc", keywords: []string{"bc", "abc"}},
		{name: "unicode", text: "这里包含敏感词和世界", keywords: []string{"世界", "敏感词"}},
		{name: "duplicates", text: "duplicate", keywords: []string{"duplicate", "DUPLICATE"}},
		{name: "empty entries", text: "blocked", keywords: []string{"", "blocked"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantKeyword, wantHit := matchBlockedKeyword(tt.text, tt.keywords)
			gotKeyword, gotHit := newContentModerationKeywordMatcher(tt.keywords).Match(tt.text)
			require.Equal(t, wantHit, gotHit)
			require.Equal(t, wantKeyword, gotKeyword)
		})
	}
}

func TestContentModerationKeywordMatcherEnglishWordBoundaries(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		keywords    []string
		wantKeyword string
		wantHit     bool
	}{
		{name: "phrase followed by longer word", text: "opposite model behavior", keywords: []string{"opposite mode"}},
		{name: "phrase followed by suffix", text: "bypass content filtering", keywords: []string{"bypass content filter"}},
		{name: "word preceded by longer word", text: "parametamorphic test", keywords: []string{"metamorphic"}},
		{name: "exact phrase", text: "switch to opposite mode now", keywords: []string{"opposite mode"}, wantKeyword: "opposite mode", wantHit: true},
		{name: "case and punctuation", text: "Detected: BYPASS CONTENT FILTER.", keywords: []string{"bypass content filter"}, wantKeyword: "bypass content filter", wantHit: true},
		{name: "mixed Chinese and English", text: "请分析make a bomb这句话", keywords: []string{"make a bomb"}, wantKeyword: "make a bomb", wantHit: true},
		{name: "Chinese keywords stay substring based", text: "这里包含敏感词条", keywords: []string{"敏感词"}, wantKeyword: "敏感词", wantHit: true},
		{name: "valid suffix survives invalid earlier match", text: "ax-bar", keywords: []string{"x-bar", "bar"}, wantKeyword: "bar", wantHit: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyword, hit := newContentModerationKeywordMatcher(tt.keywords).Match(tt.text)
			require.Equal(t, tt.wantHit, hit)
			require.Equal(t, tt.wantKeyword, keyword)
		})
	}
}

func TestContentModerationSubstringMatcherKeepsCandidateInflections(t *testing.T) {
	matcher := newContentModerationSubstringMatcher([]string{"opposite mode", "bypass content filter"})

	keyword, hit := matcher.Match("opposite model behavior")
	require.True(t, hit)
	require.Equal(t, "opposite mode", keyword)

	keyword, hit = matcher.Match("bypass content filtering")
	require.True(t, hit)
	require.Equal(t, "bypass content filter", keyword)
}

func TestContentModerationKeywordMatcherRandomizedParity(t *testing.T) {
	rng := rand.New(rand.NewSource(20260714))
	const alphabet = "abcXYZ"
	for iteration := 0; iteration < 1000; iteration++ {
		keywords := make([]string, 1+rng.Intn(30))
		for index := range keywords {
			length := 1 + rng.Intn(8)
			var value strings.Builder
			for range length {
				_ = value.WriteByte(alphabet[rng.Intn(len(alphabet))])
			}
			keywords[index] = value.String()
		}
		var text strings.Builder
		for range 20 + rng.Intn(100) {
			const textAlphabet = "abcXYZ -_."
			_ = text.WriteByte(textAlphabet[rng.Intn(len(textAlphabet))])
		}
		if iteration%2 == 0 {
			_ = text.WriteByte(' ')
			_, _ = text.WriteString(keywords[rng.Intn(len(keywords))])
			_ = text.WriteByte(' ')
		}

		wantKeyword, wantHit := matchBlockedKeyword(text.String(), keywords)
		gotKeyword, gotHit := newContentModerationKeywordMatcher(keywords).Match(text.String())
		require.Equal(t, wantHit, gotHit, "iteration %d", iteration)
		require.Equal(t, wantKeyword, gotKeyword, "iteration %d", iteration)
	}
}
