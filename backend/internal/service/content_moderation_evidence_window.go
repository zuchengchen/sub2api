package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

const (
	contentModerationEvidenceWindowBudgetRunes  = 4000
	contentModerationEvidenceWindowContext      = 1
	contentModerationEvidenceDefaultMatchRunes  = 320
	contentModerationEvidenceExpandedMatchRunes = 1024
	contentModerationEvidenceMaxWindows         = 16
	contentModerationEvidenceMaxMatches         = 64
	contentModerationEvidenceHashDomain         = "sub2api/content-moderation/evidence-window/v7\x00"
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
	Fragment               ContentModerationFragment
	Matches                []contentModerationKeywordMatch
	Tier                   string
	WholeFragment          bool
	WholeFragmentTruncated bool
}

type contentModerationEvidenceBundle struct {
	Fragment ContentModerationFragment
	Evidence moderationEvidence
	// CoverageIncomplete is stricter than Evidence.Truncated. A bounded window
	// can be truncated while still retaining every matched risk trigger.
	CoverageIncomplete bool
	// ContextIncomplete records that a contextual-review candidate could not
	// retain its complete logical fragment within the reviewer input budget.
	ContextIncomplete bool
	Windows           []ContentModerationEvidenceWindow
	AllRuleIDs        []string
	AllContexts       []string
	PrimaryKeyword    string
	PrimaryRuleID     string
	PrimaryTier       string
	CacheHash         string
	CanonicalKeys     []contentModerationEvidenceCanonicalKey
}

type contentModerationRuneSpan struct {
	start int
	end   int
}

type contentModerationCandidateSpan struct {
	contentModerationRuneSpan
	truncated bool
}

type contentModerationEvidenceCanonicalKey struct {
	Role          string
	Kind          string
	ContextClass  string
	RuleSignature string
	Text          string
}

type contentModerationCanonicalWindow struct {
	Window             ContentModerationEvidenceWindow
	Role               string
	Kind               string
	Tier               string
	RuleSignature      string
	Truncated          bool
	CoverageIncomplete bool
}

func buildContentModerationCandidateEvidence(candidates []contentModerationCandidateFragment, endpointLimit int, cfg *ContentModerationConfig) contentModerationEvidenceBundle {
	budget := contentModerationEvidenceWindowBudgetRunes
	if endpointLimit > 0 && endpointLimit < budget {
		budget = endpointLimit
	}
	if budget < 1 {
		budget = 1
	}

	allRuleIDSet := make(map[string]struct{})
	allContextSet := make(map[string]struct{})
	allRuleIDs := make([]string, 0, len(candidates))
	allContexts := make([]string, 0, len(candidates))
	primaryKeyword := ""
	primaryRuleID := ""
	primaryTier := ""

	for _, candidate := range candidates {
		contextClass := strings.TrimSpace(candidate.Fragment.ContextClass)
		if _, exists := allContextSet[contextClass]; !exists {
			allContextSet[contextClass] = struct{}{}
			allContexts = append(allContexts, contextClass)
		}
		candidateMatches := normalizedCandidateMatches(candidate.Fragment.Text, candidate.Matches)
		for _, match := range candidateMatches {
			ruleID := contentModerationKeywordRuleID(match.Keyword)
			if _, exists := allRuleIDSet[ruleID]; !exists {
				allRuleIDSet[ruleID] = struct{}{}
				allRuleIDs = append(allRuleIDs, ruleID)
			}
			if primaryKeyword == "" {
				primaryKeyword = match.Keyword
				primaryRuleID = ruleID
				primaryTier = candidate.Tier
			}
		}
	}

	canonicalWindows := make([]contentModerationCanonicalWindow, 0, len(candidates))
	canonicalIndexes := make(map[contentModerationEvidenceCanonicalKey]int, len(candidates))
	sourceTruncated := false
	coverageIncomplete := false
	contextIncomplete := false
	requiresWholeContext := false
	for _, candidate := range candidates {
		matches := normalizedCandidateMatches(candidate.Fragment.Text, candidate.Matches)
		if len(matches) == 0 {
			continue
		}

		runes := []rune(candidate.Fragment.Text)
		spans := candidateEvidenceSpans(runes, candidate.Fragment.ContextClass, matches)
		if candidate.WholeFragment {
			requiresWholeContext = true
			spans = wholeFragmentCandidateEvidenceSpan(runes, matches, budget)
		}
		if len(spans) == 0 {
			sourceTruncated = true
			coverageIncomplete = true
			continue
		}
		coveredMatches := make(map[string]struct{}, len(matches))
		for _, candidateSpan := range spans {
			span := candidateSpan.contentModerationRuneSpan
			spanTruncated := candidateSpan.truncated || candidate.WholeFragmentTruncated
			if candidate.WholeFragment && spanTruncated {
				contextIncomplete = true
			}
			spanMatches := matchesWithinSpan(matches, span)
			if len(spanMatches) == 0 {
				continue
			}
			for _, match := range spanMatches {
				coveredMatches[contentModerationSourceMatchKey(match)] = struct{}{}
			}
			rawText := strings.TrimSpace(string(runes[span.start:span.end]))
			if rawText == "" {
				sourceTruncated = true
				coverageIncomplete = true
				continue
			}
			redacted := redactContentModerationEvidenceText(rawText)
			redactedLimit := contentModerationEvidenceExpandedMatchRunes
			if candidate.WholeFragment {
				redactedLimit = budget
			}
			if boundedRedacted, redactedTruncated := cropContentModerationRedactedWindow(
				redacted, spanMatches, candidate.Tier, redactedLimit,
			); redactedTruncated {
				redacted = boundedRedacted
				spanTruncated = true
				if candidate.WholeFragment {
					contextIncomplete = true
				}
			}
			windowMatches := evidenceMatchesInRedactedWindow(redacted, spanMatches, candidate.Tier)
			windowCoverageIncomplete := !contentModerationEvidenceMatchesCoverSource(spanMatches, windowMatches)
			if len(windowMatches) < len(spanMatches) {
				spanTruncated = true
			}
			if windowCoverageIncomplete {
				// Secret redaction may replace a matched value. Keep the bounded
				// context, but never publish offsets that no longer match its text.
				spanTruncated = true
			}
			window := ContentModerationEvidenceWindow{
				Path: redactContentModerationPath(candidate.Fragment.Path), ContextClass: candidate.Fragment.ContextClass,
				Text: redacted, Matches: windowMatches,
			}
			ruleSignature := contentModerationEvidenceRuleSignature(spanMatches, candidate.Tier)
			key := contentModerationEvidenceCanonicalKey{
				Role: strings.TrimSpace(candidate.Fragment.Role), Kind: strings.TrimSpace(candidate.Fragment.Kind),
				ContextClass: strings.TrimSpace(candidate.Fragment.ContextClass), RuleSignature: ruleSignature, Text: redacted,
			}
			if existingIndex, exists := canonicalIndexes[key]; exists {
				// The first path remains the audit origin. A duplicate window does
				// not consume evidence budget, but any real source cropping remains
				// security-significant.
				canonicalWindows[existingIndex].Truncated = canonicalWindows[existingIndex].Truncated || spanTruncated
				canonicalWindows[existingIndex].CoverageIncomplete = canonicalWindows[existingIndex].CoverageIncomplete || windowCoverageIncomplete
				continue
			}
			canonicalIndexes[key] = len(canonicalWindows)
			canonicalWindows = append(canonicalWindows, contentModerationCanonicalWindow{
				Window: window, Role: candidate.Fragment.Role, Kind: candidate.Fragment.Kind, Tier: candidate.Tier,
				RuleSignature: ruleSignature, Truncated: spanTruncated, CoverageIncomplete: windowCoverageIncomplete,
			})
		}
		for _, match := range matches {
			if _, covered := coveredMatches[contentModerationSourceMatchKey(match)]; !covered {
				coverageIncomplete = true
				break
			}
		}
		if candidate.WholeFragmentTruncated {
			coverageIncomplete = true
			contextIncomplete = true
		}
	}

	windows := make([]ContentModerationEvidenceWindow, 0, len(canonicalWindows))
	segments := make([]moderationEvidenceSegment, 0, len(canonicalWindows))
	canonicalKeys := make([]contentModerationEvidenceCanonicalKey, 0, len(canonicalWindows))
	remaining := budget
	remainingMatchBudget := contentModerationEvidenceMaxMatches
	truncated := sourceTruncated
	for _, canonical := range canonicalWindows {
		if len(windows) >= contentModerationEvidenceMaxWindows || remainingMatchBudget <= 0 {
			truncated = true
			coverageIncomplete = true
			contextIncomplete = contextIncomplete || requiresWholeContext
			break
		}
		separatorRunes := 0
		if len(windows) > 0 {
			separatorRunes = 1
		}
		available := remaining - separatorRunes
		if available <= 0 {
			truncated = true
			coverageIncomplete = true
			contextIncomplete = contextIncomplete || requiresWholeContext
			break
		}

		window := canonical.Window
		windowTruncated := canonical.Truncated
		windowCoverageIncomplete := canonical.CoverageIncomplete
		if len([]rune(window.Text)) > available {
			sourceMatches := candidateMatchesFromEvidenceMatches(window.Matches)
			window.Text, _ = cropContentModerationRedactedWindow(window.Text, sourceMatches, canonical.Tier, available)
			window.Matches = evidenceMatchesInRedactedWindow(window.Text, sourceMatches, canonical.Tier)
			windowTruncated = true
			windowCoverageIncomplete = windowCoverageIncomplete || !contentModerationEvidenceMatchesCoverSource(sourceMatches, window.Matches)
			contextIncomplete = contextIncomplete || requiresWholeContext
		}
		if strings.TrimSpace(window.Text) == "" {
			truncated = true
			coverageIncomplete = true
			contextIncomplete = contextIncomplete || requiresWholeContext
			continue
		}
		if len(window.Matches) > remainingMatchBudget {
			window.Matches = window.Matches[:remainingMatchBudget]
			windowTruncated = true
			windowCoverageIncomplete = true
		}
		remainingMatchBudget -= len(window.Matches)
		if len(window.Matches) == 0 {
			windowTruncated = true
			windowCoverageIncomplete = true
		}
		coverageIncomplete = coverageIncomplete || windowCoverageIncomplete

		windows = append(windows, window)
		canonicalKeys = append(canonicalKeys, contentModerationEvidenceCanonicalKey{
			Role: strings.TrimSpace(canonical.Role), Kind: strings.TrimSpace(canonical.Kind),
			ContextClass: strings.TrimSpace(window.ContextClass), RuleSignature: canonical.RuleSignature, Text: window.Text,
		})
		segments = append(segments, moderationEvidenceSegment{
			Text: window.Text, Origin: window.Path, Role: canonical.Role, Kind: canonical.Kind,
			ContextClass: window.ContextClass, ExtractorVersion: ContentModerationEvidencePolicyVersion,
			Truncated: windowTruncated,
		})
		if windowTruncated {
			truncated = true
		}
		remaining -= separatorRunes + len([]rune(window.Text))
	}
	if len(windows) < len(canonicalWindows) {
		truncated = true
		coverageIncomplete = true
		contextIncomplete = contextIncomplete || requiresWholeContext
	}

	parts := make([]string, 0, len(windows))
	for _, window := range windows {
		parts = append(parts, window.Text)
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
		Fragment: fragment, Evidence: evidence, CoverageIncomplete: coverageIncomplete, ContextIncomplete: contextIncomplete,
		Windows: windows, AllRuleIDs: allRuleIDs, AllContexts: allContexts,
		PrimaryKeyword: primaryKeyword, PrimaryRuleID: primaryRuleID, PrimaryTier: primaryTier, CanonicalKeys: canonicalKeys,
	}
	bundle.CacheHash = contentModerationEvidenceCacheHash(bundle, cfg)
	return bundle
}

func contentModerationSourceMatchKey(match contentModerationKeywordMatch) string {
	return match.Keyword + "\x00" + itoa(match.Start) + "\x00" + itoa(match.End)
}

func contentModerationEvidenceMatchesCoverSource(source []contentModerationKeywordMatch, evidence []ContentModerationEvidenceMatch) bool {
	required := make(map[string]struct{}, len(source))
	for _, match := range source {
		required[match.Keyword] = struct{}{}
	}
	for _, match := range evidence {
		delete(required, match.Keyword)
	}
	return len(required) == 0
}

func contentModerationEvidenceRuleSignature(matches []contentModerationKeywordMatch, tier string) string {
	ruleIDSet := make(map[string]struct{}, len(matches))
	ruleIDs := make([]string, 0, len(matches))
	for _, match := range matches {
		ruleID := contentModerationKeywordRuleID(match.Keyword)
		if ruleID == "" {
			continue
		}
		if _, exists := ruleIDSet[ruleID]; exists {
			continue
		}
		ruleIDSet[ruleID] = struct{}{}
		ruleIDs = append(ruleIDs, ruleID)
	}
	sort.Strings(ruleIDs)
	return strings.TrimSpace(tier) + "\x00" + strings.Join(ruleIDs, "\x00")
}

func candidateMatchesFromEvidenceMatches(matches []ContentModerationEvidenceMatch) []contentModerationKeywordMatch {
	out := make([]contentModerationKeywordMatch, 0, len(matches))
	for _, match := range matches {
		out = append(out, contentModerationKeywordMatch{Keyword: match.Keyword, Start: match.Start, End: match.End})
	}
	return out
}

func wholeFragmentCandidateEvidenceSpan(text []rune, matches []contentModerationKeywordMatch, limit int) []contentModerationCandidateSpan {
	span := contentModerationRuneSpan{end: len(text)}
	candidate := contentModerationCandidateSpan{contentModerationRuneSpan: span}
	if limit < 1 {
		limit = 1
	}
	if len(text) > limit {
		candidate.contentModerationRuneSpan = cropCandidateSpan(span, matches, limit)
		candidate.truncated = true
	}
	return []contentModerationCandidateSpan{candidate}
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

func candidateEvidenceSpans(text []rune, contextClass string, matches []contentModerationKeywordMatch) []contentModerationCandidateSpan {
	boundaries := contentModerationSentenceSpans(text)
	switch contextClass {
	case ContentModerationContextTool, ContentModerationContextServiceLog, ContentModerationContextCode,
		ContentModerationContextConfig, ContentModerationContextUnknown:
		boundaries = contentModerationLineSpans(text)
	}
	spans := make([]contentModerationCandidateSpan, 0, len(matches))
	for _, match := range matches {
		index := containingContentModerationSpan(boundaries, match.Start, match.End)
		if index < 0 {
			spans = append(spans, contentModerationCandidateSpan{
				contentModerationRuneSpan: contentModerationRuneSpan{start: match.Start, end: match.End},
			})
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
		limit := contentModerationEvidenceDefaultMatchRunes
		if span.end-span.start > contentModerationEvidenceDefaultMatchRunes {
			limit = contentModerationEvidenceExpandedMatchRunes
		}
		candidateSpan := contentModerationCandidateSpan{contentModerationRuneSpan: span}
		if span.end-span.start > limit {
			candidateSpan.contentModerationRuneSpan = cropCandidateSpan(span, []contentModerationKeywordMatch{match}, limit)
			candidateSpan.truncated = true
		}
		spans = append(spans, candidateSpan)
	}
	return mergeContentModerationCandidateSpans(spans)
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

func mergeContentModerationCandidateSpans(spans []contentModerationCandidateSpan) []contentModerationCandidateSpan {
	if len(spans) < 2 {
		return spans
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	out := spans[:1]
	for _, span := range spans[1:] {
		last := &out[len(out)-1]
		if span.start <= last.end && span.end-last.start <= contentModerationEvidenceExpandedMatchRunes {
			if span.end > last.end {
				last.end = span.end
			}
			last.truncated = last.truncated || span.truncated
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

func cropContentModerationRedactedWindow(text string, source []contentModerationKeywordMatch, tier string, limit int) (string, bool) {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text, false
	}

	span := contentModerationRuneSpan{end: len(runes)}
	redactedMatches := evidenceMatchesInRedactedWindow(text, source, tier)
	if len(redactedMatches) > 0 {
		focus := make([]contentModerationKeywordMatch, 0, len(redactedMatches))
		for _, match := range redactedMatches {
			focus = append(focus, contentModerationKeywordMatch{
				Keyword: match.Keyword,
				Start:   match.Start,
				End:     match.End,
			})
		}
		span = cropCandidateSpan(span, focus, limit)
	} else {
		span.end = limit
	}
	return strings.TrimSpace(string(runes[span.start:span.end])), true
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
	for index, window := range bundle.Windows {
		if index < len(bundle.CanonicalKeys) {
			key := bundle.CanonicalKeys[index]
			parts = append(parts, "window", key.Role, key.Kind, key.ContextClass, key.RuleSignature, key.Text)
		} else {
			parts = append(parts, "window", window.ContextClass, window.Text)
		}
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
