package service

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const contentModerationSecondLayerPrefilterPolicyVersion = "layer2-candidate-keywords-v4-ascii-boundaries-controlled-suffixes-offsets"

var contentModerationPrefilterAllowedSuffixes = [...]string{"s", "es", "ed", "ing"}

// contentModerationPrefilterMatcher admits suspicious-looking fragments to the
// expensive second-layer model without making a moderation decision itself.
type contentModerationPrefilterMatcher struct {
	matcher           *contentModerationKeywordMatcher
	canonicalKeywords []string
}

func newContentModerationPrefilterMatcher(values []string) *contentModerationPrefilterMatcher {
	keywords := canonicalContentModerationPrefilterKeywords(values)
	if len(keywords) == 0 {
		return nil
	}
	patterns, canonicalKeywords := contentModerationPrefilterPatterns(keywords)
	return &contentModerationPrefilterMatcher{
		matcher:           newContentModerationKeywordMatcher(patterns),
		canonicalKeywords: canonicalKeywords,
	}
}

func (m *contentModerationPrefilterMatcher) Match(text string) (string, bool) {
	if m == nil || m.matcher == nil || text == "" {
		return "", false
	}
	scanner := m.scan(text, false)
	if scanner.bestKeyword < 0 || int(scanner.bestKeyword) >= len(m.canonicalKeywords) {
		return "", false
	}
	return m.canonicalKeywords[scanner.bestKeyword], true
}

func (m *contentModerationPrefilterMatcher) MatchAll(text string) []contentModerationKeywordMatch {
	if m == nil || m.matcher == nil || text == "" {
		return nil
	}
	scanner := m.scan(text, true)
	return m.canonicalizeMatches(sortAndDeduplicateContentModerationKeywordMatches(scanner.matches))
}

// MatchAllExcluding applies the output cap after allowlist suppression. The
// two automata advance over the same normalized stream, retaining candidate
// matches only until no future allowlist phrase can overlap them.
func (m *contentModerationPrefilterMatcher) MatchAllExcluding(text string, allowlist []string) []contentModerationKeywordMatch {
	excluded := newContentModerationPrefilterMatcher(allowlist)
	if m == nil || m.matcher == nil || text == "" || excluded == nil || excluded.matcher == nil {
		return m.MatchAll(text)
	}
	type pendingCandidate struct {
		match      contentModerationKeywordMatch
		suppressed bool
	}
	pending := make([]pendingCandidate, 0, 16)
	recentExcludes := make([]contentModerationKeywordMatch, 0, 16)
	out := make([]contentModerationKeywordMatch, 0, 4)
	candidateScanner := newContentModerationKeywordScanner(m.matcher, false)
	excludeScanner := newContentModerationKeywordScanner(excluded.matcher, false)
	candidateScanner.onMatch = func(match contentModerationKeywordMatch) bool {
		candidate := pendingCandidate{match: match}
		for _, excludedMatch := range recentExcludes {
			if match.normalizedStart < excludedMatch.normalizedEnd && match.normalizedEnd > excludedMatch.normalizedStart {
				candidate.suppressed = true
				break
			}
		}
		pending = append(pending, candidate)
		return true
	}
	excludeScanner.onMatch = func(match contentModerationKeywordMatch) bool {
		recentExcludes = append(recentExcludes, match)
		for index := range pending {
			candidate := pending[index].match
			if candidate.normalizedStart < match.normalizedEnd && candidate.normalizedEnd > match.normalizedStart {
				pending[index].suppressed = true
			}
		}
		return true
	}
	flush := func(normalizedEnd int, eof bool) bool {
		kept := pending[:0]
		for _, candidate := range pending {
			if !eof && normalizedEnd < candidate.match.normalizedEnd+excluded.matcher.maxPatternByteLength {
				kept = append(kept, candidate)
				continue
			}
			if !candidate.suppressed {
				out = append(out, candidate.match)
				if len(out) >= contentModerationKeywordMatchLimit {
					pending = kept
					return false
				}
			}
		}
		pending = kept
		return true
	}
	pruneExcludes := func(normalizedEnd int) {
		kept := recentExcludes[:0]
		for _, excludedMatch := range recentExcludes {
			if normalizedEnd < excludedMatch.normalizedEnd+m.matcher.maxPatternByteLength {
				kept = append(kept, excludedMatch)
			}
		}
		recentExcludes = kept
	}
	emit := func(label byte, runeStart, runeEnd int) bool {
		if !candidateScanner.emit(label, runeStart, runeEnd) || !excludeScanner.emit(label, runeStart, runeEnd) {
			return false
		}
		if !flush(candidateScanner.position, false) {
			return false
		}
		pruneExcludes(candidateScanner.position)
		return true
	}

	lastSpace := false
	emitted := false
	originalRuneIndex := 0
	keepScanning := true
	for _, original := range text {
		lower := unicode.ToLower(original)
		if unicode.IsLetter(lower) || unicode.IsDigit(lower) {
			var encoded [utf8.UTFMax]byte
			length := utf8.EncodeRune(encoded[:], lower)
			for index := 0; index < length; index++ {
				if !emit(encoded[index], originalRuneIndex, originalRuneIndex+1) {
					keepScanning = false
					break
				}
			}
			lastSpace = false
			emitted = true
		} else if emitted && !lastSpace {
			keepScanning = emit(' ', originalRuneIndex, originalRuneIndex+1)
			lastSpace = true
		}
		originalRuneIndex++
		if !keepScanning {
			break
		}
	}
	if keepScanning {
		candidateScanner.finish()
		excludeScanner.finish()
		_ = flush(candidateScanner.position, true)
	}
	return m.canonicalizeMatches(sortAndDeduplicateContentModerationKeywordMatches(out))
}

func (m *contentModerationPrefilterMatcher) canonicalizeMatches(matches []contentModerationKeywordMatch) []contentModerationKeywordMatch {
	for index := range matches {
		keywordIndex := matches[index].keywordIndex
		if keywordIndex >= 0 && keywordIndex < len(m.canonicalKeywords) {
			matches[index].Keyword = m.canonicalKeywords[keywordIndex]
		}
	}
	return matches
}

func (m *contentModerationPrefilterMatcher) scan(text string, collect bool) *contentModerationKeywordScanner {
	scanner := newContentModerationKeywordScanner(m.matcher, collect)
	lastSpace := false
	emitted := false
	originalRuneIndex := 0
	for _, original := range text {
		lower := unicode.ToLower(original)
		if unicode.IsLetter(lower) || unicode.IsDigit(lower) {
			if !emitLowerContentModerationRune(scanner, lower, originalRuneIndex, originalRuneIndex+1) {
				break
			}
			lastSpace = false
			emitted = true
		} else if emitted && !lastSpace {
			if !scanner.emit(' ', originalRuneIndex, originalRuneIndex+1) {
				break
			}
			lastSpace = true
		}
		originalRuneIndex++
	}
	scanner.finish()
	return scanner
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

// contentModerationPrefilterPatterns keeps configured terms authoritative and
// adds only a small set of direct final-token suffixes. Generated patterns map
// back to the configured term so audit rule identities remain stable.
func contentModerationPrefilterPatterns(keywords []string) ([]string, []string) {
	patterns := make([]string, 0, len(keywords)*2)
	canonicalKeywords := make([]string, 0, len(keywords)*2)
	seen := make(map[string]struct{}, len(keywords)*2)
	for _, keyword := range keywords {
		seen[keyword] = struct{}{}
		patterns = append(patterns, keyword)
		canonicalKeywords = append(canonicalKeywords, keyword)
	}
	for _, keyword := range keywords {
		if !contentModerationPrefilterAllowsSuffixes(keyword) {
			continue
		}
		for _, suffix := range contentModerationPrefilterAllowedSuffixes {
			pattern := keyword + suffix
			if _, exists := seen[pattern]; exists {
				continue
			}
			seen[pattern] = struct{}{}
			patterns = append(patterns, pattern)
			canonicalKeywords = append(canonicalKeywords, keyword)
		}
	}
	return patterns, canonicalKeywords
}

func contentModerationPrefilterAllowsSuffixes(keyword string) bool {
	finalToken := keyword
	if separator := strings.LastIndexByte(keyword, ' '); separator >= 0 {
		finalToken = keyword[separator+1:]
	}
	if len(finalToken) < 3 {
		return false
	}
	for index := range len(finalToken) {
		if finalToken[index] < 'a' || finalToken[index] > 'z' {
			return false
		}
	}
	return true
}

func normalizeContentModerationPrefilterText(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	lastSpace := false
	for _, original := range value {
		lower := unicode.ToLower(original)
		if unicode.IsLetter(lower) || unicode.IsDigit(lower) {
			_, _ = builder.WriteRune(lower)
			lastSpace = false
		} else if !lastSpace && builder.Len() > 0 {
			_ = builder.WriteByte(' ')
			lastSpace = true
		}
	}
	normalized := builder.String()
	if lastSpace && len(normalized) > 0 {
		normalized = normalized[:len(normalized)-1]
	}
	return normalized
}
