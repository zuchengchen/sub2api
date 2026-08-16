package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

const (
	contentModerationEvidenceWindowBudgetRunes = 1024
	contentModerationEvidenceWindowContext     = 1
	contentModerationEvidencePerMatchRunes     = 320
	contentModerationEvidenceMaxWindows        = 16
	contentModerationEvidenceMaxMatches        = 64
	contentModerationEvidenceHashDomain        = "sub2api/content-moderation/evidence-window/v2\x00"
)

// ContentModerationEvidenceMatch offsets are Unicode rune indexes relative to
// the redacted Text of the containing evidence window.
type ContentModerationEvidenceMatch struct {
	Keyword string `json:"keyword"`
	RuleID  string `json:"rule_id"`
	Tier    string `json:"tier"`
	Start   int    `json:"start"`
	End     int    `json:"end"`
}

// ContentModerationEvidenceWindow is both the bounded audit representation
// and the exact redacted content supplied to the second-layer model.
type ContentModerationEvidenceWindow struct {
	Path         string                           `json:"path"`
	ContextClass string                           `json:"context_class"`
	Text         string                           `json:"text"`
	Matches      []ContentModerationEvidenceMatch `json:"matches"`
}

type contentModerationCandidateFragment struct {
	Fragment ContentModerationFragment
	Matches  []contentModerationKeywordMatch
	Tier     string
}

type contentModerationEvidenceBundle struct {
	Fragment       ContentModerationFragment
	Evidence       moderationEvidence
	Windows        []ContentModerationEvidenceWindow
	AllRuleIDs     []string
	AllContexts    []string
	PrimaryKeyword string
	PrimaryRuleID  string
	CacheHash      string
}

type contentModerationRuneSpan struct {
	start int
	end   int
}

func buildContentModerationCandidateEvidence(candidates []contentModerationCandidateFragment, endpointLimit int, cfg *ContentModerationConfig) contentModerationEvidenceBundle {
	budget := contentModerationEvidenceWindowBudgetRunes
	if endpointLimit > 0 && endpointLimit < budget {
		budget = endpointLimit
	}
	if budget < 1 {
		budget = 1
	}

	windows := make([]ContentModerationEvidenceWindow, 0, len(candidates))
	segments := make([]moderationEvidenceSegment, 0, len(candidates))
	allRuleIDSet := make(map[string]struct{})
	allContextSet := make(map[string]struct{})
	allRuleIDs := make([]string, 0, len(candidates))
	allContexts := make([]string, 0, len(candidates))
	primaryKeyword := ""
	primaryRuleID := ""
	remaining := budget
	remainingMatchBudget := contentModerationEvidenceMaxMatches
	truncated := false
	totalCandidateMatches := 0

	for _, candidate := range candidates {
		contextClass := strings.TrimSpace(candidate.Fragment.ContextClass)
		if _, exists := allContextSet[contextClass]; !exists {
			allContextSet[contextClass] = struct{}{}
			allContexts = append(allContexts, contextClass)
		}
		candidateMatches := normalizedCandidateMatches(candidate.Fragment.Text, candidate.Matches)
		totalCandidateMatches += len(candidateMatches)
		for _, match := range candidateMatches {
			ruleID := contentModerationKeywordRuleID(match.Keyword)
			if _, exists := allRuleIDSet[ruleID]; !exists {
				allRuleIDSet[ruleID] = struct{}{}
				allRuleIDs = append(allRuleIDs, ruleID)
			}
			if primaryKeyword == "" {
				primaryKeyword = match.Keyword
				primaryRuleID = ruleID
			}
		}
	}

	for _, candidate := range candidates {
		matches := normalizedCandidateMatches(candidate.Fragment.Text, candidate.Matches)
		if len(matches) == 0 {
			continue
		}

		runes := []rune(candidate.Fragment.Text)
		spans := candidateEvidenceSpans(runes, candidate.Fragment.ContextClass, matches)
		for _, span := range spans {
			if len(windows) >= contentModerationEvidenceMaxWindows || remainingMatchBudget <= 0 {
				truncated = true
				break
			}
			if remaining <= 0 {
				truncated = true
				break
			}
			spanMatches := matchesWithinSpan(matches, span)
			if len(spanMatches) == 0 {
				continue
			}
			originalSpan := span
			if span.end-span.start > remaining {
				span = cropCandidateSpan(span, spanMatches, remaining)
				truncated = true
			}
			rawText := strings.TrimSpace(string(runes[span.start:span.end]))
			if rawText == "" {
				continue
			}
			redacted := redactContentModerationEvidenceText(rawText)
			if redactedRunes := []rune(redacted); len(redactedRunes) > remaining {
				redacted = string(redactedRunes[:remaining])
				truncated = true
			}
			windowMatches := evidenceMatchesInRedactedWindow(redacted, spanMatches, candidate.Tier)
			if len(windowMatches) > remainingMatchBudget {
				windowMatches = windowMatches[:remainingMatchBudget]
				truncated = true
			}
			remainingMatchBudget -= len(windowMatches)
			if len(windowMatches) == 0 {
				// Secret redaction may replace a matched value. Keep the bounded
				// context, but never publish offsets that no longer match its text.
				truncated = true
			}
			window := ContentModerationEvidenceWindow{
				Path: redactContentModerationPath(candidate.Fragment.Path), ContextClass: candidate.Fragment.ContextClass,
				Text: redacted, Matches: windowMatches,
			}
			windows = append(windows, window)
			segments = append(segments, moderationEvidenceSegment{
				Text: redacted, Origin: window.Path, Role: candidate.Fragment.Role, Kind: candidate.Fragment.Kind,
				ContextClass: candidate.Fragment.ContextClass, ExtractorVersion: ContentModerationEvidencePolicyVersion,
				Truncated: originalSpan != span,
			})
			remaining -= len([]rune(redacted))
			if remaining > 0 {
				remaining-- // Account for the newline inserted between windows.
			}
		}
		if remaining <= 0 {
			break
		}
	}

	parts := make([]string, 0, len(windows))
	shownMatches := 0
	for _, window := range windows {
		parts = append(parts, window.Text)
		shownMatches += len(window.Matches)
	}
	if shownMatches < totalCandidateMatches {
		truncated = true
	}
	evidenceText := strings.Join(parts, "\n")
	role := "tool"
	kind := "candidate_evidence"
	path := "moderation.evidence_windows"
	contextClass := ContentModerationContextUnknown
	if len(candidates) > 0 {
		role = candidates[0].Fragment.Role
		kind = candidates[0].Fragment.Kind
		path = candidates[0].Fragment.Path
		contextClass = candidates[0].Fragment.ContextClass
		for _, candidate := range candidates[1:] {
			if candidate.Fragment.ContextClass != contextClass {
				role = "tool"
				kind = "candidate_evidence"
				path = "moderation.evidence_windows"
				contextClass = ContentModerationContextUnknown
				break
			}
		}
	}
	fragment, _ := newContentModerationFragment(role, kind, path, evidenceText)
	fragment.ContextClass = contextClass
	evidence := moderationEvidence{Text: evidenceText, Mode: "keyword_windows", Truncated: truncated, Segments: segments}
	sort.Strings(allRuleIDs)
	sort.Strings(allContexts)
	bundle := contentModerationEvidenceBundle{
		Fragment: fragment, Evidence: evidence, Windows: windows, AllRuleIDs: allRuleIDs, AllContexts: allContexts,
		PrimaryKeyword: primaryKeyword, PrimaryRuleID: primaryRuleID,
	}
	bundle.CacheHash = contentModerationEvidenceCacheHash(bundle, cfg)
	return bundle
}

func normalizedCandidateMatches(text string, matches []contentModerationKeywordMatch) []contentModerationKeywordMatch {
	maxRunes := len([]rune(text))
	out := append([]contentModerationKeywordMatch(nil), matches...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		if out[i].End != out[j].End {
			return out[i].End < out[j].End
		}
		return out[i].Keyword < out[j].Keyword
	})
	valid := out[:0]
	for _, match := range out {
		if match.Start < 0 || match.End <= match.Start || match.End > maxRunes || strings.TrimSpace(match.Keyword) == "" {
			continue
		}
		if len(valid) > 0 {
			last := valid[len(valid)-1]
			if last.Start == match.Start && last.End == match.End && last.Keyword == match.Keyword {
				continue
			}
		}
		valid = append(valid, match)
	}
	return valid
}

func candidateEvidenceSpans(text []rune, contextClass string, matches []contentModerationKeywordMatch) []contentModerationRuneSpan {
	boundaries := contentModerationSentenceSpans(text)
	switch contextClass {
	case ContentModerationContextTool, ContentModerationContextServiceLog, ContentModerationContextCode,
		ContentModerationContextConfig, ContentModerationContextUnknown:
		boundaries = contentModerationLineSpans(text)
	}
	spans := make([]contentModerationRuneSpan, 0, len(matches))
	for _, match := range matches {
		index := containingContentModerationSpan(boundaries, match.Start, match.End)
		if index < 0 {
			spans = append(spans, contentModerationRuneSpan{start: match.Start, end: match.End})
			continue
		}
		first := index - contentModerationEvidenceWindowContext
		if first < 0 {
			first = 0
		}
		last := index + contentModerationEvidenceWindowContext
		if last >= len(boundaries) {
			last = len(boundaries) - 1
		}
		span := contentModerationRuneSpan{start: boundaries[first].start, end: boundaries[last].end}
		if span.end-span.start > contentModerationEvidencePerMatchRunes {
			span = cropCandidateSpan(span, []contentModerationKeywordMatch{match}, contentModerationEvidencePerMatchRunes)
		}
		spans = append(spans, span)
	}
	return mergeContentModerationRuneSpans(spans)
}

func contentModerationLineSpans(text []rune) []contentModerationRuneSpan {
	spans := make([]contentModerationRuneSpan, 0, strings.Count(string(text), "\n")+1)
	start := 0
	for index, r := range text {
		if r == '\n' {
			spans = append(spans, contentModerationRuneSpan{start: start, end: index + 1})
			start = index + 1
		}
	}
	if start < len(text) || len(spans) == 0 {
		spans = append(spans, contentModerationRuneSpan{start: start, end: len(text)})
	}
	return spans
}

func contentModerationSentenceSpans(text []rune) []contentModerationRuneSpan {
	spans := make([]contentModerationRuneSpan, 0, 4)
	start := 0
	for index, r := range text {
		switch r {
		case '.', '?', '!', ';', '\n', '\r', '。', '？', '！', '；':
			spans = append(spans, contentModerationRuneSpan{start: start, end: index + 1})
			start = index + 1
		}
	}
	if start < len(text) || len(spans) == 0 {
		spans = append(spans, contentModerationRuneSpan{start: start, end: len(text)})
	}
	return spans
}

func containingContentModerationSpan(spans []contentModerationRuneSpan, start, end int) int {
	for index, span := range spans {
		if start < span.end && end > span.start {
			return index
		}
	}
	return -1
}

func mergeContentModerationRuneSpans(spans []contentModerationRuneSpan) []contentModerationRuneSpan {
	if len(spans) < 2 {
		return spans
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	out := spans[:1]
	for _, span := range spans[1:] {
		last := &out[len(out)-1]
		if span.start <= last.end {
			if span.end > last.end {
				last.end = span.end
			}
			continue
		}
		out = append(out, span)
	}
	return out
}

func matchesWithinSpan(matches []contentModerationKeywordMatch, span contentModerationRuneSpan) []contentModerationKeywordMatch {
	out := make([]contentModerationKeywordMatch, 0, len(matches))
	for _, match := range matches {
		if match.Start < span.end && match.End > span.start {
			out = append(out, match)
		}
	}
	return out
}

func cropCandidateSpan(span contentModerationRuneSpan, matches []contentModerationKeywordMatch, limit int) contentModerationRuneSpan {
	if limit <= 0 || span.end-span.start <= limit {
		return span
	}
	focusStart := matches[0].Start
	focusEnd := matches[0].End
	for _, match := range matches[1:] {
		if match.End-focusStart > limit {
			break
		}
		focusEnd = match.End
	}
	padding := limit - (focusEnd - focusStart)
	if padding < 0 {
		padding = 0
	}
	start := focusStart - padding/2
	if start < span.start {
		start = span.start
	}
	end := start + limit
	if end > span.end {
		end = span.end
		start = end - limit
	}
	return contentModerationRuneSpan{start: start, end: end}
}

func redactContentModerationEvidenceText(text string) string {
	text = evidenceAuthorizationPattern.ReplaceAllString(text, "$1$2[REDACTED]")
	text = evidenceSecretPattern.ReplaceAllString(text, "$1$2[REDACTED]")
	return redactContentModerationSecrets(text)
}

func evidenceMatchesInRedactedWindow(text string, source []contentModerationKeywordMatch, tier string) []ContentModerationEvidenceMatch {
	keywords := make([]string, 0, len(source))
	seen := make(map[string]struct{}, len(source))
	for _, match := range source {
		if _, exists := seen[match.Keyword]; exists {
			continue
		}
		seen[match.Keyword] = struct{}{}
		keywords = append(keywords, match.Keyword)
	}
	matches := newContentModerationPrefilterMatcher(keywords).MatchAll(text)
	if len(matches) > contentModerationEvidenceMaxMatches {
		matches = matches[:contentModerationEvidenceMaxMatches]
	}
	out := make([]ContentModerationEvidenceMatch, 0, len(matches))
	for _, match := range matches {
		out = append(out, ContentModerationEvidenceMatch{
			Keyword: match.Keyword, RuleID: contentModerationKeywordRuleID(match.Keyword), Tier: tier, Start: match.Start, End: match.End,
		})
	}
	return out
}

func contentModerationEvidenceCacheHash(bundle contentModerationEvidenceBundle, cfg *ContentModerationConfig) string {
	parts := []string{contentModerationEvidenceHashDomain, ContentModerationEvidencePolicyVersion, bundle.Evidence.Text}
	for _, window := range bundle.Windows {
		parts = append(parts, window.Path, window.ContextClass, window.Text)
		for _, match := range window.Matches {
			parts = append(parts, match.Keyword, match.RuleID, match.Tier, itoa(match.Start), itoa(match.End))
		}
	}
	parts = append(parts, "rule_ids")
	parts = append(parts, bundle.AllRuleIDs...)
	parts = append(parts, "contexts")
	parts = append(parts, bundle.AllContexts...)
	if cfg != nil {
		for _, endpoint := range cfg.enabledSecondLayerEndpoints() {
			parts = append(parts, endpoint.ID, endpoint.Model, endpoint.Profile, endpoint.PromptVersion)
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])
}
