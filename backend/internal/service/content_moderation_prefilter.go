package service

import (
	"strings"
	"unicode"
)

const contentModerationSecondLayerPrefilterPolicyVersion = "layer2-prefilter-v1"

// contentModerationPrefilterMatcher admits suspicious-looking fragments to the
// expensive second-layer model without making a moderation decision itself.
type contentModerationPrefilterMatcher struct {
	matcher *contentModerationKeywordMatcher
}

func newContentModerationPrefilterMatcher(values []string) *contentModerationPrefilterMatcher {
	keywords := canonicalContentModerationPrefilterKeywords(values)
	if len(keywords) == 0 {
		return nil
	}
	return &contentModerationPrefilterMatcher{matcher: newContentModerationSubstringMatcher(keywords)}
}

func (m *contentModerationPrefilterMatcher) Match(text string) (string, bool) {
	if m == nil || m.matcher == nil {
		return "", false
	}
	return m.matcher.Match(normalizeContentModerationPrefilterText(text))
}

func canonicalContentModerationPrefilterKeywords(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	keywords := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeContentModerationPrefilterText(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		keywords = append(keywords, value)
	}
	return keywords
}

func normalizeContentModerationPrefilterText(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	lastSpace := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			_, _ = builder.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			_ = builder.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(builder.String())
}
