package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var contentModerationBodySizeUpperBounds = [...]int64{
	64 << 10,
	1 << 20,
	16 << 20,
	64 << 20,
	256 << 20,
	-1,
}

const contentModerationWhitelistShadowCacheSuffix = ":whitelist-shadow-v1"

const (
	contentModerationShadowQueueCapacity    = 64
	contentModerationContextScopeMaxRunes   = 4096
	contentModerationContextScopeMaxBytes   = contentModerationContextScopeMaxRunes * utf8.UTFMax
	contentModerationContextScopeMaxWindows = 64
)

type contentModerationFragmentScope struct {
	Fragments []ContentModerationFragment
	Owner     int
	Active    bool
	Truncated bool
}

type contentModerationCheckFragment struct {
	Fragment               ContentModerationFragment
	WholeFragment          bool
	WholeFragmentTruncated bool
	CacheEligible          bool
}

func (s *ContentModerationService) ReservePendingRequestBody(bytes int64) (*ContentModerationPendingReservation, bool) {
	if s == nil {
		return nil, true
	}
	s.ensurePendingBodyBudget()
	if s.pendingBodyBudgetBytes.Load() <= 0 {
		s.pendingBodyBudgetBytes.CompareAndSwap(0, DefaultContentModerationPendingBodyBudgetBytes)
	}
	updateAtomicMaximum(&s.observedRequestBodyMax, bytes)
	s.recordContentModerationBodySize(bytes)
	limit := s.pendingBodyBudgetBytes.Load()
	return s.pendingBodyBudget.TryReserve(bytes, limit)
}

func (s *ContentModerationService) ensurePendingBodyBudget() {
	if s == nil {
		return
	}
	s.pendingBodyBudgetOnce.Do(func() {
		if s.pendingBodyBudget == nil {
			s.pendingBodyBudget = NewContentModerationPendingBodyBudget()
		}
		if s.pendingBodyBudgetBytes.Load() <= 0 {
			s.pendingBodyBudgetBytes.Store(DefaultContentModerationPendingBodyBudgetBytes)
		}
	})
}

func (s *ContentModerationService) recordContentModerationBodySize(bytes int64) {
	if s == nil {
		return
	}
	if bytes < 0 {
		bytes = 0
	}
	for index, upper := range contentModerationBodySizeUpperBounds {
		if upper < 0 || bytes <= upper {
			s.requestBodyBuckets[index].Add(1)
			return
		}
	}
}

func (s *ContentModerationService) contentModerationBodySizeHistogram() []ContentModerationBodySizeBucket {
	items := make([]ContentModerationBodySizeBucket, len(contentModerationBodySizeUpperBounds))
	for index, upper := range contentModerationBodySizeUpperBounds {
		items[index] = ContentModerationBodySizeBucket{UpperBoundBytes: upper, Count: s.requestBodyBuckets[index].Load()}
	}
	return items
}

// SetPendingRequestBodyBudgetForTest overrides the budget for deterministic
// concurrency tests. It must be called before reservations are acquired.
func (s *ContentModerationService) SetPendingRequestBodyBudgetForTest(bytes int64) {
	if s == nil || bytes <= 0 {
		return
	}
	s.ensurePendingBodyBudget()
	s.pendingBodyBudgetBytes.Store(bytes)
}

func (s *ContentModerationService) PendingRequestBodyBytes() int64 {
	if s == nil {
		return 0
	}
	s.ensurePendingBodyBudget()
	return s.pendingBodyBudget.InUse()
}

func (s *ContentModerationService) checkUnifiedFragments(ctx context.Context, input ContentModerationCheckInput, runtime *contentModerationRuntimeSnapshot) *ContentModerationDecision {
	allow := &ContentModerationDecision{Allowed: true, Action: ContentModerationActionAllow}
	if input.Scope == nil || !input.Scope.InScope {
		return allow
	}
	if runtime == nil || !runtime.riskControlEnabled || runtime.config == nil {
		return allow
	}
	cfg := runtime.config
	if !cfg.Enabled || cfg.Mode == ContentModerationModeOff {
		return allow
	}
	whitelistShadow := cfg.includesUserEmail(input.UserEmail)

	fragments := ExtractContentModerationFragments(input.Protocol, input.Body)
	if len(fragments) == 0 {
		return allow
	}
	var scopeKeywordMatcher *contentModerationKeywordMatcher
	if cfg.KeywordBlockingMode != ContentModerationKeywordModeAPIOnly {
		scopeKeywordMatcher = runtime.keywordMatcher
	}
	var scopeCandidateMatcher *contentModerationPrefilterMatcher
	if cfg.SecondLayerEnabled && cfg.KeywordBlockingMode != ContentModerationKeywordModeKeywordOnly {
		scopeCandidateMatcher = runtime.secondLayerPrefilterMatcher
	}
	fragmentScopes := buildContentModerationFragmentScopes(
		fragments, scopeKeywordMatcher, scopeCandidateMatcher, cfg.KeywordAllowlist,
	)
	checkFragments := buildContentModerationCheckFragments(fragments, fragmentScopes)
	boundaryFragments := buildContentModerationCrossMessageBoundaryFragments(
		fragments, scopeKeywordMatcher, scopeCandidateMatcher, cfg.KeywordAllowlist,
	)
	if len(boundaryFragments) > 0 {
		checkFragments = append(boundaryFragments, checkFragments...)
	}
	cache, _ := s.hashCache.(ContentModerationFragmentCache)
	namespace := runtime.fragmentCacheNamespace
	if namespace == "" {
		namespace = cfg.fragmentCacheNamespace()
	}
	if whitelistShadow {
		namespace += contentModerationWhitelistShadowCacheSuffix
	}
	candidates := make([]contentModerationCandidateFragment, 0, len(checkFragments))
	for _, checkFragment := range checkFragments {
		fragment := checkFragment.Fragment
		fragmentCacheEligible := checkFragment.CacheEligible
		shadowRiskObserved := false
		releaseDecisionLock := s.acquireContentModerationFragmentDecisionLock(namespace + "\x00" + fragment.Hash)
		if cache != nil && fragmentCacheEligible {
			entry, found, err := s.getUnifiedFragmentCache(ctx, cache, namespace, fragment.Hash)
			if err != nil {
				s.fragmentCacheErrors.Add(1)
				slog.Warn("content_moderation.fragment_cache_get_failed", "error", err)
			} else if !found {
				if entry.Expired {
					s.fragmentCacheExpired.Add(1)
				}
				s.fragmentCacheMisses.Add(1)
			} else {
				s.fragmentCacheHits.Add(1)
				switch entry.Result {
				case ContentModerationFragmentAllow:
					releaseDecisionLock()
					continue
				case ContentModerationFragmentBlock:
					s.fragmentCacheReplays.Add(1)
					category := entry.Category
					keyword := entry.MatchedKeyword
					if category == "" && cfg.KeywordBlockingMode != ContentModerationKeywordModeAPIOnly && runtime.keywordMatcher != nil {
						if matched, hit := runtime.keywordMatcher.Match(fragment.Text); hit {
							category = contentModerationKeywordCategory
							keyword = matched
						}
					}
					category = defaultContentModerationString(category, "fragment_cache")
					cachedEvidence := contentModerationEvidenceBundle{}
					if keyword != "" && runtime.keywordMatcher != nil {
						cachedEvidence = buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
							Fragment: fragment,
							Matches:  contentModerationHardMatchesForKeyword(fragment.Text, keyword),
							Tier:     "high_confidence",
						}}, contentModerationEvidenceWindowBudgetRunes, cfg)
					}
					evidenceMode := entry.EvidenceMode
					if cachedEvidence.Evidence.Mode != "" {
						evidenceMode = cachedEvidence.Evidence.Mode
					}
					evidenceTruncated := entry.EvidenceTruncated || cachedEvidence.Evidence.Truncated
					if whitelistShadow {
						shadowRiskObserved = true
						s.persistUnifiedShadowAudit(ctx, input, cfg, fragment, ContentModerationActionWhitelistShadow, category, keyword, unifiedModerationAudit{
							CacheHit: true, DecisionSource: "cache_replay_whitelist_shadow", SourceLogID: entry.SourceLogID,
							ReplayOfInputHash: defaultContentModerationString(entry.ReplayOfInputHash, fragment.Hash), CacheNamespace: namespace,
							ModelProfile: entry.ModelProfile, PromptVersion: entry.PromptVersion, EvidenceMode: evidenceMode,
							EvidenceTruncated: evidenceTruncated, ParserStatus: entry.ParserStatus,
							KeywordTier: entry.KeywordTier, KeywordRuleID: entry.KeywordRuleID,
							EvidenceText: cachedEvidence.Evidence.Text, EvidenceWindows: cachedEvidence.Windows,
						})
						releaseDecisionLock()
						continue
					}
					decision, _, _ := s.unifiedBlockDecisionWithAudit(ctx, input, cfg, fragment, ContentModerationActionCacheBlock, category, keyword, unifiedModerationAudit{
						CacheHit: true, DecisionSource: "cache_replay", SourceLogID: entry.SourceLogID,
						ReplayOfInputHash: defaultContentModerationString(entry.ReplayOfInputHash, fragment.Hash), CacheNamespace: namespace,
						ModelProfile: entry.ModelProfile, PromptVersion: entry.PromptVersion, EvidenceMode: evidenceMode,
						EvidenceTruncated: evidenceTruncated, ParserStatus: entry.ParserStatus,
						KeywordTier: entry.KeywordTier, KeywordRuleID: entry.KeywordRuleID,
						EvidenceText: cachedEvidence.Evidence.Text, EvidenceWindows: cachedEvidence.Windows,
					})
					releaseDecisionLock()
					return decision
				}
			}
		}

		var contextualMatches []contentModerationKeywordMatch
		if cfg.KeywordBlockingMode != ContentModerationKeywordModeAPIOnly && runtime.keywordMatcher != nil {
			keyword, hardMatches, reviewMatches := classifyUnifiedHardKeywordMatches(fragment, runtime)
			if keyword == "" && len(reviewMatches) == 0 && checkFragment.WholeFragmentTruncated && runtime.contextualKeywordMatcher != nil {
				reviewMatches = runtime.contextualKeywordMatcher.MatchAll(fragment.Text)
			}
			if keyword == "" && len(reviewMatches) > 0 &&
				(!cfg.SecondLayerEnabled || cfg.KeywordBlockingMode == ContentModerationKeywordModeKeywordOnly) {
				keyword = reviewMatches[0].Keyword
				hardMatches = reviewMatches
				reviewMatches = nil
			}
			contextualMatches = reviewMatches
			if keyword != "" {
				hardEvidence := buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
					Fragment: fragment, Matches: hardMatches, Tier: "high_confidence",
				}}, contentModerationEvidenceWindowBudgetRunes, cfg)
				audit := unifiedModerationAudit{
					DecisionSource: "keyword_high_confidence", CacheNamespace: namespace, KeywordTier: "high_confidence", KeywordRuleID: contentModerationKeywordRuleID(keyword),
					EvidenceMode: hardEvidence.Evidence.Mode, EvidenceTruncated: hardEvidence.Evidence.Truncated,
					EvidenceText: hardEvidence.Evidence.Text, EvidenceWindows: hardEvidence.Windows,
				}
				if whitelistShadow {
					shadowRiskObserved = true
					audit.DecisionSource = "keyword_high_confidence_whitelist_shadow"
					s.persistUnifiedShadowAudit(ctx, input, cfg, fragment, ContentModerationActionWhitelistShadow, contentModerationKeywordCategory, keyword, audit)
				} else if cfg.FirstLayerStage == ContentModerationFirstLayerStageShadow {
					shadowRiskObserved = true
					audit.DecisionSource = "keyword_high_confidence_shadow"
					audit.KeywordTier = "first_layer_shadow"
					s.persistUnifiedShadowAudit(ctx, input, cfg, fragment, ContentModerationActionFirstLayerShadow, contentModerationKeywordCategory, keyword, audit)
				} else {
					decision, log, persisted := s.unifiedBlockDecisionWithAudit(ctx, input, cfg, fragment, ContentModerationActionKeywordBlock, contentModerationKeywordCategory, keyword, audit)
					if persisted && fragmentCacheEligible {
						s.putUnifiedFragmentCacheEntry(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentCacheEntry{
							Result: ContentModerationFragmentBlock, SourceLogID: contentModerationLogIDPtr(log), ReplayOfInputHash: fragment.Hash,
							DecisionSource: "keyword_high_confidence", Category: contentModerationKeywordCategory, MatchedKeyword: keyword,
							KeywordTier: "high_confidence", KeywordRuleID: contentModerationKeywordRuleID(keyword),
							EvidenceMode: hardEvidence.Evidence.Mode, EvidenceTruncated: hardEvidence.Evidence.Truncated,
						})
					}
					releaseDecisionLock()
					return decision
				}
			}
		}
		if len(contextualMatches) > 0 {
			// Contextual hard-keyword decisions depend on the complete logical
			// fragment. If bounded evidence cannot represent it, the review must
			// remain truncated and fail closed instead of caching a partial allow.
			candidates = appendOrMergeContentModerationCandidate(candidates, contentModerationCandidateFragment{
				Fragment: fragment, Matches: contextualMatches, Tier: "contextual_review",
				WholeFragment: true, WholeFragmentTruncated: checkFragment.WholeFragmentTruncated,
			})
		}

		if cfg.SecondLayerEnabled && cfg.KeywordBlockingMode != ContentModerationKeywordModeKeywordOnly {
			if len(contextualMatches) > 0 && runtime.secondLayerPrefilterMatcher == nil {
				releaseDecisionLock()
				continue
			}
			candidateSystemReady := runtime.secondLayerPrefilterMatcher != nil
			if !candidateSystemReady {
				slog.Warn("content_moderation.candidate_system_unavailable", "request_id", input.RequestID)
				releaseDecisionLock()
				continue
			}
			candidateMatches := runtime.secondLayerPrefilterMatcher.MatchAllExcluding(fragment.Text, cfg.KeywordAllowlist)
			if len(candidateMatches) > 0 {
				tier := "candidate"
				if checkFragment.WholeFragmentTruncated {
					tier = "contextual_review"
				}
				candidates = appendOrMergeContentModerationCandidate(candidates, contentModerationCandidateFragment{
					Fragment: fragment, Matches: candidateMatches, Tier: tier,
					WholeFragment: checkFragment.WholeFragment, WholeFragmentTruncated: checkFragment.WholeFragmentTruncated,
				})
				releaseDecisionLock()
				continue
			}
			if len(contextualMatches) > 0 {
				releaseDecisionLock()
				continue
			}
			if fragmentCacheEligible && !whitelistShadow && !shadowRiskObserved {
				s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
			}
			releaseDecisionLock()
			continue
		}
		if len(contextualMatches) > 0 {
			releaseDecisionLock()
			continue
		}

		if fragmentCacheEligible && !shadowRiskObserved {
			s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
		}
		releaseDecisionLock()
	}
	if len(candidates) > 0 {
		return s.checkUnifiedCandidateEvidence(ctx, input, cfg, namespace, cache, candidates, whitelistShadow)
	}
	return allow
}

func classifyUnifiedHardKeywordMatches(
	fragment ContentModerationFragment,
	runtime *contentModerationRuntimeSnapshot,
) (string, []contentModerationKeywordMatch, []contentModerationKeywordMatch) {
	if runtime == nil || runtime.keywordMatcher == nil {
		return "", nil, nil
	}
	firstKeyword, hit := runtime.keywordMatcher.Match(fragment.Text)
	if !hit {
		return "", nil, nil
	}
	unconditional := runtime.unconditionalKeywordMatcher
	contextual := runtime.contextualKeywordMatcher
	if unconditional == nil && contextual == nil {
		_, unconditional, contextual = newContentModerationRuntimeKeywordMatchers(runtime.keywordMatcher.keywords)
	}
	_, firstIsContextual := maliciousMacroContextKeywords[normalizedContentModerationKeywordKey(firstKeyword)]
	if !firstIsContextual && !suppressToolDocumentationKeyword(fragment, firstKeyword) {
		return firstKeyword, contentModerationHardMatchesForKeyword(fragment.Text, firstKeyword), nil
	}
	if unconditional != nil {
		matchText := fragment.Text
		keyword, hardHit := unconditional.Match(matchText)
		if hardHit && suppressToolDocumentationKeyword(fragment, keyword) {
			matchText = withoutPowerShellDocumentationCommands(matchText)
			keyword, hardHit = unconditional.Match(matchText)
		}
		if hardHit {
			return keyword, contentModerationHardMatchesForKeyword(matchText, keyword), nil
		}
	}
	if contextual == nil {
		return "", nil, nil
	}
	reviewMatches := make([]contentModerationKeywordMatch, 0, 4)
	for _, keyword := range contextual.keywords {
		matches := contentModerationHardMatchesForKeyword(fragment.Text, keyword)
		if len(matches) == 0 {
			continue
		}
		disposition, configured := classifyContentModerationKeywordContext(fragment, keyword)
		if !configured || disposition == contentModerationKeywordContextHardBlock {
			return keyword, matches, reviewMatches
		}
		if disposition == contentModerationKeywordContextReview {
			reviewMatches = append(reviewMatches, matches...)
		}
	}
	return "", nil, sortAndDeduplicateContentModerationKeywordMatches(reviewMatches)
}

func buildContentModerationFragmentScopes(
	fragments []ContentModerationFragment,
	keywordMatcher *contentModerationKeywordMatcher,
	candidateMatcher *contentModerationPrefilterMatcher,
	keywordAllowlist []string,
) []contentModerationFragmentScope {
	scopes := make([]contentModerationFragmentScope, len(fragments))
	if keywordMatcher == nil && candidateMatcher == nil {
		return scopes
	}
	type fragmentGroup struct {
		role    string
		path    string
		members []int
	}
	groups := make(map[string]*fragmentGroup)
	orderedGroups := make([]*fragmentGroup, 0)
	for index, fragment := range fragments {
		key, path, ok := contentModerationInstructionGroupKey(fragment)
		if !ok {
			continue
		}
		group := groups[key]
		if group == nil {
			group = &fragmentGroup{role: fragment.Role, path: path}
			groups[key] = group
			orderedGroups = append(orderedGroups, group)
		}
		group.members = append(group.members, index)
	}

	for _, group := range orderedGroups {
		if len(group.members) < 2 {
			continue
		}
		groupFragments := make([]ContentModerationFragment, 0, len(group.members))
		for _, index := range group.members {
			groupFragments = append(groupFragments, fragments[index])
		}
		if contentModerationFragmentGroupBytes(groupFragments) > contentModerationContextScopeMaxBytes {
			scopeFragments := buildLargeContentModerationScopeFragments(
				group.role, group.path, groupFragments, keywordMatcher, candidateMatcher, keywordAllowlist,
			)
			if len(scopeFragments) == 0 {
				continue
			}
			owner := group.members[0]
			for _, index := range group.members {
				scopes[index] = contentModerationFragmentScope{
					Fragments: scopeFragments, Owner: owner, Active: true, Truncated: true,
				}
			}
			continue
		}
		text, matches, ok := contentModerationFragmentScopeText(groupFragments, keywordMatcher, candidateMatcher, keywordAllowlist)
		if !ok {
			continue
		}
		scopeFragments, truncated := newContentModerationScopeFragments(group.role, group.path, text, matches)
		if len(scopeFragments) == 0 {
			continue
		}
		owner := group.members[0]
		for _, index := range group.members {
			scopes[index] = contentModerationFragmentScope{
				Fragments: scopeFragments, Owner: owner, Active: true, Truncated: truncated,
			}
		}
	}
	return scopes
}

type contentModerationMessageUnit struct {
	role    string
	root    string
	index   int
	path    string
	members []int
}

func buildContentModerationCrossMessageBoundaryFragments(
	fragments []ContentModerationFragment,
	keywordMatcher *contentModerationKeywordMatcher,
	candidateMatcher *contentModerationPrefilterMatcher,
	keywordAllowlist []string,
) []contentModerationCheckFragment {
	if len(fragments) < 2 || (keywordMatcher == nil && candidateMatcher == nil) {
		return nil
	}
	unitsByKey := make(map[string]*contentModerationMessageUnit)
	units := make([]*contentModerationMessageUnit, 0, len(fragments))
	for fragmentIndex, fragment := range fragments {
		role, root, messageIndex, path, ok := contentModerationFragmentMessageUnit(fragment)
		if !ok {
			continue
		}
		key := root + "\x00" + strconv.Itoa(messageIndex)
		unit := unitsByKey[key]
		if unit == nil {
			unit = &contentModerationMessageUnit{role: role, root: root, index: messageIndex, path: path}
			unitsByKey[key] = unit
			units = append(units, unit)
		}
		unit.members = append(unit.members, fragmentIndex)
	}

	items := make([]contentModerationCheckFragment, 0, 4)
	seen := make(map[string]struct{}, 4)
	for index := 1; index < len(units); index++ {
		left := units[index-1]
		right := units[index]
		if left.root != right.root || right.index != left.index+1 || left.role != right.role ||
			!contentModerationInstructionRole(left.role) || len(left.members) == 0 || len(right.members) == 0 {
			continue
		}
		leftFragment := fragments[left.members[len(left.members)-1]]
		rightFragment := fragments[right.members[0]]
		boundaryTruncated := len(left.members) != 1 || len(right.members) != 1 ||
			len(leftFragment.Text) > contentModerationContextScopeMaxRunes/2 ||
			len(rightFragment.Text) > contentModerationContextScopeMaxRunes/2
		for _, text := range contentModerationCrossMessageBoundaryTexts(
			leftFragment, rightFragment, keywordMatcher, candidateMatcher, keywordAllowlist,
		) {
			if len(items) >= contentModerationContextScopeMaxWindows {
				for itemIndex := range items {
					items[itemIndex].WholeFragmentTruncated = true
				}
				return items
			}
			fragment, ok := newContentModerationFragment(left.role, "text", left.path+".boundary."+strconv.Itoa(right.index), text)
			if !ok {
				continue
			}
			fragment.ContextClass = ContentModerationContextUser
			updateContentModerationFragmentHash(&fragment)
			if _, exists := seen[fragment.Hash]; exists {
				continue
			}
			seen[fragment.Hash] = struct{}{}
			items = append(items, contentModerationCheckFragment{
				Fragment: fragment, WholeFragment: true, WholeFragmentTruncated: boundaryTruncated,
			})
		}
	}
	return items
}

func contentModerationInstructionRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user", "developer", "system":
		return true
	default:
		return false
	}
}

func contentModerationFragmentMessageUnit(fragment ContentModerationFragment) (string, string, int, string, bool) {
	if strings.ToLower(strings.TrimSpace(fragment.Kind)) != "text" {
		return "", "", 0, "", false
	}
	parts := strings.Split(strings.TrimSpace(fragment.Path), ".")
	if len(parts) < 2 || !contentModerationPathPartIsIndex(parts[1]) {
		return "", "", 0, "", false
	}
	root := parts[0]
	switch root {
	case "messages", "contents":
	case "input":
		if len(parts) < 3 || parts[2] != "content" {
			return "", "", 0, "", false
		}
	default:
		return "", "", 0, "", false
	}
	messageIndex, err := strconv.Atoi(parts[1])
	if err != nil || messageIndex < 0 {
		return "", "", 0, "", false
	}
	role := strings.ToLower(strings.TrimSpace(fragment.Role))
	return role, root, messageIndex, root + "." + parts[1], true
}

func contentModerationCrossMessageBoundaryTexts(
	left ContentModerationFragment,
	right ContentModerationFragment,
	keywordMatcher *contentModerationKeywordMatcher,
	candidateMatcher *contentModerationPrefilterMatcher,
	keywordAllowlist []string,
) []string {
	texts := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	leftContext := contentModerationSuffixBytes(left.Text, contentModerationContextScopeMaxRunes/2)
	rightContext := contentModerationPrefixBytes(right.Text, contentModerationContextScopeMaxRunes/2)
	addIfTriggered := func(boundary, reviewText string) {
		matches := contentModerationScopeTriggerMatches(boundary, keywordMatcher, candidateMatcher, keywordAllowlist)
		if len(matches) == 0 {
			return
		}
		triggerKeywords := make(map[string]struct{}, len(matches))
		for _, match := range matches {
			triggerKeywords[normalizedContentModerationKeywordKey(match.Keyword)] = struct{}{}
		}
		reviewText = strings.TrimSpace(reviewText)
		if reviewText == "" {
			return
		}
		reviewKeepsTrigger := false
		for _, match := range contentModerationScopeTriggerMatches(reviewText, keywordMatcher, candidateMatcher, keywordAllowlist) {
			if _, exists := triggerKeywords[normalizedContentModerationKeywordKey(match.Keyword)]; exists {
				reviewKeepsTrigger = true
				break
			}
		}
		if !reviewKeepsTrigger {
			reviewText = strings.TrimSpace(boundary)
		}
		if _, exists := seen[reviewText]; exists {
			return
		}
		seen[reviewText] = struct{}{}
		texts = append(texts, reviewText)
	}

	if limit := contentModerationScopeBoundaryBytes(keywordMatcher, nil); limit > 0 {
		leftText := contentModerationSuffixBytes(left.Text, limit)
		rightText := contentModerationPrefixBytes(right.Text, limit)
		addIfTriggered(leftText+" "+rightText, leftContext+" "+rightContext)
		addIfTriggered(leftText+rightText, leftContext+rightContext)
	}
	if candidateMatcher != nil {
		limit := maxContentModerationBlockedKeywordRunes + 2
		leftText, _ := contentModerationPrefilterNormalizedSuffix(left.Text, limit)
		rightText := contentModerationPrefilterNormalizedPrefix(right.Text, limit)
		addIfTriggered(leftText+" "+rightText, leftContext+" "+rightContext)
		addIfTriggered(leftText+rightText, leftContext+rightContext)
	}
	return texts
}

func contentModerationFragmentGroupBytes(fragments []ContentModerationFragment) int {
	total := 0
	for _, fragment := range fragments {
		if len(fragment.Text) > contentModerationContextScopeMaxBytes-total {
			return contentModerationContextScopeMaxBytes + 1
		}
		total += len(fragment.Text)
	}
	return total
}

func buildLargeContentModerationScopeFragments(
	role string,
	path string,
	fragments []ContentModerationFragment,
	keywordMatcher *contentModerationKeywordMatcher,
	candidateMatcher *contentModerationPrefilterMatcher,
	keywordAllowlist []string,
) []ContentModerationFragment {
	windowTexts := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	addWindow := func(text string) bool {
		text = strings.TrimSpace(text)
		if text == "" {
			return true
		}
		if _, exists := seen[text]; exists {
			return true
		}
		seen[text] = struct{}{}
		windowTexts = append(windowTexts, text)
		return len(windowTexts) < contentModerationContextScopeMaxWindows
	}

	if len(windowTexts) < contentModerationContextScopeMaxWindows {
		limit := contentModerationScopeBoundaryBytes(keywordMatcher, nil)
		if limit > 0 && len(fragments) > 1 {
			tails := [3]string{
				contentModerationSuffixBytes(fragments[0].Text, limit),
				contentModerationSuffixBytes(fragments[0].Text, limit),
				contentModerationSuffixBytes(fragments[0].Text, limit),
			}
			for index := 1; index < len(fragments) && len(windowTexts) < contentModerationContextScopeMaxWindows; index++ {
				fragment := fragments[index]
				prefix := contentModerationPrefixBytes(fragment.Text, limit)
				rawSeparator := ""
				if fragments[index-1].trailingSpace || fragment.leadingSpace {
					rawSeparator = " "
				}
				separators := [3]string{rawSeparator, " ", ""}
				for variant := range tails {
					boundary := tails[variant] + separators[variant] + prefix
					if len(contentModerationScopeTriggerMatches(boundary, keywordMatcher, nil, nil)) > 0 && !addWindow(boundary) {
						break
					}
				}
				for variant := range tails {
					if len(fragment.Text) >= limit {
						tails[variant] = contentModerationSuffixBytes(fragment.Text, limit)
						continue
					}
					tails[variant] = contentModerationSuffixBytes(tails[variant]+separators[variant]+fragment.Text, limit)
				}
			}
		}
	}

	// Original fragments are scanned again by the normal request loop. Only
	// full layer-one terms need a synthetic window here: contextual terms need
	// sibling-aware fail-closed review, while unconditional terms are retained
	// to prioritize them ahead of lower-risk boundary candidates.
	for _, fragment := range fragments {
		matches := contentModerationScopeTriggerMatches(fragment.Text, keywordMatcher, nil, nil)
		for _, window := range contentModerationRuneWindows(fragment.Text, matches, contentModerationContextScopeMaxRunes) {
			if !addWindow(window) {
				break
			}
		}
		if len(windowTexts) >= contentModerationContextScopeMaxWindows {
			break
		}
	}

	if candidateMatcher != nil && len(windowTexts) < contentModerationContextScopeMaxWindows && len(fragments) > 1 {
		limit := maxContentModerationBlockedKeywordRunes + 2
		firstTail, _ := contentModerationPrefilterNormalizedSuffix(fragments[0].Text, limit)
		tails := [3]string{firstTail, firstTail, firstTail}
		for index := 1; index < len(fragments) && len(windowTexts) < contentModerationContextScopeMaxWindows; index++ {
			fragment := fragments[index]
			prefix := contentModerationPrefilterNormalizedPrefix(fragment.Text, limit)
			rawSeparator := ""
			if fragments[index-1].trailingSpace || fragment.leadingSpace {
				rawSeparator = " "
			}
			separators := [3]string{rawSeparator, " ", ""}
			for variant := range tails {
				boundary := tails[variant] + separators[variant] + prefix
				if len(candidateMatcher.MatchAllExcluding(boundary, keywordAllowlist)) > 0 && !addWindow(boundary) {
					break
				}
			}
			fragmentTail, saturated := contentModerationPrefilterNormalizedSuffix(fragment.Text, limit)
			for variant := range tails {
				if saturated {
					tails[variant] = fragmentTail
					continue
				}
				tails[variant], _ = contentModerationPrefilterNormalizedSuffix(tails[variant]+separators[variant]+fragmentTail, limit)
			}
		}
	}

	scopeFragments := make([]ContentModerationFragment, 0, len(windowTexts))
	for _, windowText := range windowTexts {
		fragment, ok := newContentModerationFragment(role, "text", path, windowText)
		if !ok {
			continue
		}
		fragment.ContextClass = ContentModerationContextUser
		updateContentModerationFragmentHash(&fragment)
		scopeFragments = append(scopeFragments, fragment)
	}
	return scopeFragments
}

func contentModerationScopeBoundaryBytes(
	keywordMatcher *contentModerationKeywordMatcher,
	candidateMatcher *contentModerationPrefilterMatcher,
) int {
	limit := 0
	if keywordMatcher != nil {
		limit = keywordMatcher.maxPatternByteLength
	}
	if candidateMatcher != nil && candidateMatcher.matcher != nil && candidateMatcher.matcher.maxPatternByteLength > limit {
		limit = candidateMatcher.matcher.maxPatternByteLength
	}
	if limit > contentModerationContextScopeMaxBytes/2 {
		limit = contentModerationContextScopeMaxBytes / 2
	}
	return limit
}

func contentModerationPrefixBytes(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

func contentModerationSuffixBytes(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

func contentModerationRuneWindows(text string, matches []contentModerationKeywordMatch, limit int) []string {
	if limit <= 0 || len(matches) == 0 {
		return nil
	}
	type runeWindow struct {
		startRune int
		endRune   int
		startByte int
		endByte   int
	}
	windows := make([]runeWindow, len(matches))
	for index, match := range matches {
		start := match.Start - limit/2
		if start < 0 {
			start = 0
		}
		windows[index] = runeWindow{startRune: start, endRune: start + limit, endByte: len(text)}
	}
	startIndex := 0
	endIndex := 0
	runeIndex := 0
	for byteIndex := range text {
		for startIndex < len(windows) && windows[startIndex].startRune == runeIndex {
			windows[startIndex].startByte = byteIndex
			startIndex++
		}
		for endIndex < len(windows) && windows[endIndex].endRune == runeIndex {
			windows[endIndex].endByte = byteIndex
			endIndex++
		}
		runeIndex++
	}
	texts := make([]string, 0, len(windows))
	for _, window := range windows {
		texts = append(texts, text[window.startByte:window.endByte])
	}
	return texts
}

func contentModerationPrefilterNormalizedPrefix(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	out := make([]rune, 0, limit)
	lastSpace := false
	emitted := false
	for _, original := range text {
		lower := unicode.ToLower(original)
		if unicode.IsLetter(lower) || unicode.IsDigit(lower) {
			out = append(out, lower)
			lastSpace = false
			emitted = true
		} else if emitted && !lastSpace {
			out = append(out, ' ')
			lastSpace = true
		}
		if len(out) >= limit {
			break
		}
	}
	return string(out)
}

func contentModerationPrefilterNormalizedSuffix(text string, limit int) (string, bool) {
	if limit <= 0 {
		return "", false
	}
	ring := make([]rune, limit)
	start := 0
	count := 0
	total := 0
	lastSpace := false
	emitted := false
	appendRune := func(value rune) {
		if count < limit {
			ring[(start+count)%limit] = value
			count++
		} else {
			ring[start] = value
			start = (start + 1) % limit
		}
		total++
	}
	for _, original := range text {
		lower := unicode.ToLower(original)
		if unicode.IsLetter(lower) || unicode.IsDigit(lower) {
			appendRune(lower)
			lastSpace = false
			emitted = true
		} else if emitted && !lastSpace {
			appendRune(' ')
			lastSpace = true
		}
	}
	out := make([]rune, count)
	for index := range out {
		out[index] = ring[(start+index)%limit]
	}
	return string(out), total >= limit
}

func contentModerationFragmentScopeText(
	fragments []ContentModerationFragment,
	keywordMatcher *contentModerationKeywordMatcher,
	candidateMatcher *contentModerationPrefilterMatcher,
	keywordAllowlist []string,
) (string, []contentModerationKeywordMatch, bool) {
	if len(fragments) < 2 {
		return "", nil, false
	}
	texts := make([]string, 0, len(fragments))
	for _, fragment := range fragments {
		texts = append(texts, fragment.Text)
	}
	rawJoined := joinContentModerationFragmentsAtOriginalWhitespace(fragments)
	spaceJoined := strings.Join(texts, " ")
	boundaryless := strings.Join(texts, "")
	type scopeVariant struct {
		text    string
		matches []contentModerationKeywordMatch
	}
	variants := make([]scopeVariant, 0, 3)
	seenTexts := make(map[string]struct{}, 3)
	for _, text := range []string{rawJoined, spaceJoined, boundaryless} {
		if _, exists := seenTexts[text]; exists {
			continue
		}
		seenTexts[text] = struct{}{}
		matches := contentModerationScopeTriggerMatches(text, keywordMatcher, candidateMatcher, keywordAllowlist)
		if len(matches) > 0 {
			variants = append(variants, scopeVariant{text: text, matches: matches})
		}
	}
	if len(variants) == 0 {
		return "", nil, false
	}
	selected := variants[0].text
	selectedMatches := variants[0].matches
	for _, variant := range variants[1:] {
		if !contentModerationMatchKeywordSetsEqual(selectedMatches, variant.matches) {
			parts := make([]string, 0, len(variants))
			for _, current := range variants {
				parts = append(parts, current.text)
			}
			// Retain both interpretations only when fragment boundaries expose
			// different rules. This closes both word-boundary and mid-word splits
			// without duplicating normal multipart messages in reviewer evidence.
			selected = strings.Join(parts, "\n")
			selectedMatches = contentModerationScopeTriggerMatches(selected, keywordMatcher, candidateMatcher, keywordAllowlist)
			break
		}
	}
	if len(selectedMatches) == 0 {
		return "", nil, false
	}
	return selected, selectedMatches, true
}

func joinContentModerationFragmentsAtOriginalWhitespace(fragments []ContentModerationFragment) string {
	var combined strings.Builder
	for index, fragment := range fragments {
		if index > 0 && (fragments[index-1].trailingSpace || fragment.leadingSpace) {
			combined.WriteByte(' ')
		}
		combined.WriteString(fragment.Text)
	}
	return combined.String()
}

func newContentModerationScopeFragments(
	role string,
	path string,
	text string,
	matches []contentModerationKeywordMatch,
) ([]ContentModerationFragment, bool) {
	runes := []rune(text)
	truncated := len(runes) > contentModerationContextScopeMaxRunes
	windowTexts := []string{text}
	if truncated {
		windowTexts = make([]string, 0, len(matches))
		seen := make(map[string]struct{}, len(matches))
		for _, match := range matches {
			start := match.Start - contentModerationContextScopeMaxRunes/2
			if start < 0 {
				start = 0
			}
			end := start + contentModerationContextScopeMaxRunes
			if end > len(runes) {
				end = len(runes)
				start = end - contentModerationContextScopeMaxRunes
			}
			windowText := string(runes[start:end])
			if _, exists := seen[windowText]; exists {
				continue
			}
			seen[windowText] = struct{}{}
			windowTexts = append(windowTexts, windowText)
		}
	}
	fragments := make([]ContentModerationFragment, 0, len(windowTexts))
	for _, windowText := range windowTexts {
		fragment, ok := newContentModerationFragment(role, "text", path, windowText)
		if !ok {
			continue
		}
		fragment.ContextClass = ContentModerationContextUser
		updateContentModerationFragmentHash(&fragment)
		fragments = append(fragments, fragment)
	}
	return fragments, truncated
}

func contentModerationScopeTriggerMatches(
	text string,
	keywordMatcher *contentModerationKeywordMatcher,
	candidateMatcher *contentModerationPrefilterMatcher,
	keywordAllowlist []string,
) []contentModerationKeywordMatch {
	matches := make([]contentModerationKeywordMatch, 0, 4)
	if keywordMatcher != nil {
		matches = append(matches, keywordMatcher.MatchAll(text)...)
	}
	if candidateMatcher != nil {
		matches = append(matches, candidateMatcher.MatchAllExcluding(text, keywordAllowlist)...)
	}
	return sortAndDeduplicateContentModerationKeywordMatches(matches)
}

func contentModerationMatchKeywordSetsEqual(left, right []contentModerationKeywordMatch) bool {
	leftKeywords := make(map[string]struct{}, len(left))
	rightKeywords := make(map[string]struct{}, len(right))
	for _, match := range left {
		leftKeywords[normalizedContentModerationKeywordKey(match.Keyword)] = struct{}{}
	}
	for _, match := range right {
		rightKeywords[normalizedContentModerationKeywordKey(match.Keyword)] = struct{}{}
	}
	if len(leftKeywords) != len(rightKeywords) {
		return false
	}
	for keyword := range leftKeywords {
		if _, exists := rightKeywords[keyword]; !exists {
			return false
		}
	}
	return true
}

func buildContentModerationCheckFragments(fragments []ContentModerationFragment, scopes []contentModerationFragmentScope) []contentModerationCheckFragment {
	items := make([]contentModerationCheckFragment, 0, len(fragments)+1)
	for index, fragment := range fragments {
		if index >= len(scopes) || !scopes[index].Active {
			items = append(items, contentModerationCheckFragment{Fragment: fragment, CacheEligible: true})
			continue
		}
		scope := scopes[index]
		if !scope.Truncated {
			if index == scope.Owner {
				for _, scopeFragment := range scope.Fragments {
					items = append(items, contentModerationCheckFragment{
						Fragment: scopeFragment, WholeFragment: true, CacheEligible: true,
					})
				}
			}
			continue
		}

		// A bounded composite cannot replace original first-layer and candidate
		// scans. Keep every original member, then add one fail-closed composite
		// item that detects contextual keywords split across member boundaries.
		items = append(items, contentModerationCheckFragment{Fragment: fragment})
		if index == scope.Owner {
			for _, scopeFragment := range scope.Fragments {
				items = append(items, contentModerationCheckFragment{
					Fragment: scopeFragment, WholeFragment: true, WholeFragmentTruncated: true,
				})
			}
		}
	}
	return items
}

func contentModerationInstructionGroupKey(fragment ContentModerationFragment) (string, string, bool) {
	if strings.ToLower(strings.TrimSpace(fragment.Kind)) != "text" {
		return "", "", false
	}
	role := strings.ToLower(strings.TrimSpace(fragment.Role))
	if role != "user" && role != "developer" && role != "system" {
		return "", "", false
	}
	trustedMetadata := ContentModerationFragment{Role: role, Kind: fragment.Kind, Path: fragment.Path}
	if classifyContentModerationContext(trustedMetadata) != ContentModerationContextUser {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSpace(fragment.Path), ".")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", false
	}
	root := parts[0]
	path := ""
	switch root {
	case "messages", "contents":
		if len(parts) < 2 || !contentModerationPathPartIsIndex(parts[1]) {
			return "", "", false
		}
		path = root + "." + parts[1]
	case "input":
		if len(parts) == 1 {
			path = root
		} else if !contentModerationPathPartIsIndex(parts[1]) {
			path = root
		} else if len(parts) == 2 {
			// Bare input array strings are one logical Responses input value.
			path = root
		} else {
			path = root + "." + parts[1]
		}
	case "system", "system_instruction", "systemInstruction", "developer", "instructions", "prompt", "negative_prompt":
		path = root
	default:
		return "", "", false
	}
	return role + "\x00" + path, path, true
}

func contentModerationPathPartIsIndex(value string) bool {
	if value == "" {
		return false
	}
	for _, current := range value {
		if current < '0' || current > '9' {
			return false
		}
	}
	return true
}

func appendOrMergeContentModerationCandidate(candidates []contentModerationCandidateFragment, candidate contentModerationCandidateFragment) []contentModerationCandidateFragment {
	for index := range candidates {
		existing := &candidates[index]
		if existing.Tier != candidate.Tier || existing.Fragment.Hash != candidate.Fragment.Hash ||
			existing.WholeFragment != candidate.WholeFragment ||
			existing.WholeFragmentTruncated != candidate.WholeFragmentTruncated {
			continue
		}
		existing.Matches = sortAndDeduplicateContentModerationKeywordMatches(append(existing.Matches, candidate.Matches...))
		return candidates
	}
	return append(candidates, candidate)
}

func normalizedContentModerationKeywordKey(keyword string) string {
	return strings.ToLower(strings.TrimSpace(keyword))
}

func contentModerationHardMatchesForKeyword(text, keyword string) []contentModerationKeywordMatch {
	matcher := newContentModerationKeywordMatcher([]string{strings.TrimSpace(keyword)})
	if matcher == nil {
		return nil
	}
	return matcher.MatchAll(text)
}

func (s *ContentModerationService) checkUnifiedCandidateEvidence(
	ctx context.Context,
	input ContentModerationCheckInput,
	cfg *ContentModerationConfig,
	namespace string,
	cache ContentModerationFragmentCache,
	candidates []contentModerationCandidateFragment,
	whitelistShadow bool,
) *ContentModerationDecision {
	allow := &ContentModerationDecision{Allowed: true, Action: ContentModerationActionAllow}
	reviewRequired := contentModerationCandidatesRequireContextualReview(candidates)
	if reviewRequired {
		candidates = interleaveContextualReviewCandidates(candidates)
	}
	endpoints := cfg.enabledSecondLayerEndpoints()
	limit := contentModerationEvidenceWindowBudgetRunes
	if len(endpoints) > 0 {
		limit = endpoints[0].InputLimit
		for _, endpoint := range endpoints[1:] {
			if endpoint.InputLimit < limit {
				limit = endpoint.InputLimit
			}
		}
	}
	bundle := buildContentModerationCandidateEvidence(candidates, limit, cfg)
	primary := candidates[0].Fragment
	if strings.TrimSpace(bundle.Evidence.Text) == "" || bundle.CacheHash == "" {
		slog.Warn("content_moderation.candidate_evidence_empty", "request_id", input.RequestID)
		if reviewRequired {
			return s.handleContextualReviewUnavailable(ctx, input, cfg, namespace, bundle, primary, whitelistShadow, "not_attempted", errors.New("contextual review evidence is empty"))
		}
		return allow
	}
	if len(endpoints) == 0 {
		if reviewRequired {
			return s.handleContextualReviewUnavailable(ctx, input, cfg, namespace, bundle, primary, whitelistShadow, "not_attempted", errors.New("contextual review endpoint is unavailable"))
		}
		return allow
	}
	if whitelistShadow || cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow {
		asyncInput := input
		// Shadow decisions never archive or mutate the request. Keep only the
		// scalar metadata needed for the audit row so queued work cannot retain
		// a large gateway body or its reservation after the request returns.
		asyncInput.Body = nil
		asyncInput.RawRequest.Body = nil
		asyncInput.RawRequest.Headers = nil
		asyncInput.Reservation = nil
		primary.Text = bundle.Evidence.Text
		asyncCfg := cloneContentModerationConfig(cfg)
		enqueued := s.enqueueContentModerationShadowReview(func() {
			timeout := contentModerationShadowReviewTimeout(asyncCfg)
			shadowCtx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			_ = s.checkUnifiedCandidateEvidenceBundle(shadowCtx, asyncInput, asyncCfg, namespace, cache, bundle, primary, whitelistShadow, reviewRequired)
		})
		if !enqueued {
			slog.Warn("content_moderation.second_layer_shadow_queue_full", "request_id", input.RequestID)
			if reviewRequired {
				return s.handleContextualReviewUnavailable(ctx, input, cfg, namespace, bundle, primary, whitelistShadow, "busy", errContentModerationSecondLayerBusy)
			}
		}
		return allow
	}
	return s.checkUnifiedCandidateEvidenceBundle(ctx, input, cfg, namespace, cache, bundle, primary, whitelistShadow, reviewRequired)
}

func (s *ContentModerationService) checkUnifiedCandidateEvidenceBundle(
	ctx context.Context,
	input ContentModerationCheckInput,
	cfg *ContentModerationConfig,
	namespace string,
	cache ContentModerationFragmentCache,
	bundle contentModerationEvidenceBundle,
	primary ContentModerationFragment,
	whitelistShadow bool,
	reviewRequired bool,
) *ContentModerationDecision {
	allow := &ContentModerationDecision{Allowed: true, Action: ContentModerationActionAllow}
	cacheFragment := bundle.Fragment
	cacheFragment.Hash = bundle.CacheHash
	cacheFragment.Path = "moderation.evidence_windows"
	releaseDecisionLock := s.acquireContentModerationFragmentDecisionLock(namespace + "\x00" + bundle.CacheHash)
	defer releaseDecisionLock()

	cacheEligible := !(reviewRequired && bundle.Evidence.Truncated)
	if cache != nil && cacheEligible {
		entry, found, err := s.getUnifiedFragmentCache(ctx, cache, namespace, bundle.CacheHash)
		if err != nil {
			s.fragmentCacheErrors.Add(1)
			slog.Warn("content_moderation.evidence_cache_get_failed", "error", err)
		} else if found {
			s.fragmentCacheHits.Add(1)
			if entry.Result == ContentModerationFragmentAllow {
				return allow
			}
			if entry.Result == ContentModerationFragmentBlock {
				s.fragmentCacheReplays.Add(1)
				category := defaultContentModerationString(entry.Category, "fragment_cache")
				audit := unifiedModerationAudit{
					CacheHit: true, DecisionSource: "cache_replay", SourceLogID: entry.SourceLogID,
					ReplayOfInputHash: defaultContentModerationString(entry.ReplayOfInputHash, bundle.CacheHash),
					CacheNamespace:    namespace, ModelProfile: entry.ModelProfile, PromptVersion: entry.PromptVersion,
					EvidenceMode: entry.EvidenceMode, EvidenceTruncated: entry.EvidenceTruncated,
					ParserStatus: entry.ParserStatus, KeywordTier: entry.KeywordTier, KeywordRuleID: entry.KeywordRuleID,
					EvidenceText: bundle.Evidence.Text, EvidenceWindows: bundle.Windows, InputHash: bundle.CacheHash,
				}
				if whitelistShadow {
					audit.DecisionSource = "cache_replay_whitelist_shadow"
					s.persistUnifiedShadowAudit(ctx, input, cfg, primary, ContentModerationActionWhitelistShadow, category, bundle.PrimaryKeyword, audit)
					return allow
				}
				decision, _, _ := s.unifiedBlockDecisionWithAudit(ctx, input, cfg, primary, ContentModerationActionCacheBlock, category, bundle.PrimaryKeyword, audit)
				return decision
			}
		} else {
			if entry.Expired {
				s.fragmentCacheExpired.Add(1)
			}
			s.fragmentCacheMisses.Add(1)
		}
	}

	keywordTier := "candidate"
	if reviewRequired {
		keywordTier = "contextual_review"
	}
	result, attempted, err := s.scanUnifiedSecondLayerPrepared(ctx, cfg, contentModerationSecondLayerInput{
		Fragment: bundle.Fragment, Evidence: bundle.Evidence, KeywordTier: keywordTier, KeywordRuleID: bundle.PrimaryRuleID,
		Background: whitelistShadow || cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow,
	})
	if err != nil {
		if !errors.Is(err, errContentModerationSecondLayerBusy) {
			slog.Warn("content_moderation.second_layer_failed", "error", err)
		}
		if reviewRequired {
			return s.handleContextualReviewUnavailable(ctx, input, cfg, namespace, bundle, primary, whitelistShadow, contentModerationContextualReviewFailureStatus(err, true), err)
		}
		return allow
	}
	if !attempted {
		if reviewRequired {
			return s.handleContextualReviewUnavailable(ctx, input, cfg, namespace, bundle, primary, whitelistShadow, "not_attempted", errors.New("contextual review was not attempted"))
		}
		return allow
	}
	audit := unifiedModerationAudit{
		DecisionSource: "model", CacheNamespace: namespace, ModelProfile: result.Profile,
		PromptVersion: result.PromptVersion, EvidenceMode: result.EvidenceMode,
		EvidenceTruncated: result.EvidenceTruncated, ParserStatus: result.ParserStatus,
		KeywordTier: result.KeywordTier, KeywordRuleID: result.KeywordRuleID,
		EvidenceText: bundle.Evidence.Text, EvidenceWindows: bundle.Windows, InputHash: bundle.CacheHash,
	}
	if result.Blocked {
		if whitelistShadow || cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow {
			action := ContentModerationActionSecondLayerShadow
			audit.DecisionSource = "model_shadow"
			if whitelistShadow {
				action = ContentModerationActionWhitelistShadow
				audit.DecisionSource = "model_whitelist_shadow"
			}
			s.persistUnifiedShadowAudit(ctx, input, cfg, primary, action, result.Category, bundle.PrimaryKeyword, audit)
			return allow
		}
		decision, log, persisted := s.unifiedBlockDecisionWithAudit(ctx, input, cfg, primary, ContentModerationActionSecondLayerBlock, result.Category, bundle.PrimaryKeyword, audit)
		if persisted && cacheEligible {
			s.putUnifiedFragmentCacheEntry(ctx, cache, namespace, cfg, cacheFragment, ContentModerationFragmentCacheEntry{
				Result: ContentModerationFragmentBlock, SourceLogID: contentModerationLogIDPtr(log), ReplayOfInputHash: bundle.CacheHash,
				DecisionSource: "model", Category: result.Category, MatchedKeyword: bundle.PrimaryKeyword,
				ModelProfile: result.Profile, PromptVersion: result.PromptVersion, KeywordTier: result.KeywordTier,
				KeywordRuleID: result.KeywordRuleID, EvidenceMode: result.EvidenceMode,
				EvidenceTruncated: result.EvidenceTruncated, ParserStatus: result.ParserStatus,
			})
		}
		return decision
	}
	if reviewRequired && bundle.Evidence.Truncated {
		return s.handleContextualReviewUnavailable(ctx, input, cfg, namespace, bundle, primary, whitelistShadow, "evidence_truncated", errors.New("contextual review evidence was truncated"))
	}

	if cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow || cfg.RecordNonHits {
		action := ContentModerationActionAllow
		if cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow {
			action = ContentModerationActionSecondLayerShadow
			audit.DecisionSource = "model_shadow"
		}
		log := s.buildLog(input, cfg, action, false, "", 0, nil, bundle.Evidence.Text, nil, nil, "")
		log.MatchedKeyword = bundle.PrimaryKeyword
		applyUnifiedModerationAudit(log, primary, cfg, audit)
		s.persistContentModerationLogWithInput(ctx, cfg, log, bundle.CacheHash, false, false, &input)
	}
	if !whitelistShadow {
		s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, cacheFragment, ContentModerationFragmentAllow)
	}
	return allow
}

func contentModerationCandidatesRequireContextualReview(candidates []contentModerationCandidateFragment) bool {
	for _, candidate := range candidates {
		if candidate.Tier == "contextual_review" {
			return true
		}
	}
	return false
}

func interleaveContextualReviewCandidates(candidates []contentModerationCandidateFragment) []contentModerationCandidateFragment {
	contextual := make([]contentModerationCandidateFragment, 0, len(candidates))
	ordinary := make([]contentModerationCandidateFragment, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Tier == "contextual_review" {
			contextual = append(contextual, candidate)
		} else {
			ordinary = append(ordinary, candidate)
		}
	}
	ordered := make([]contentModerationCandidateFragment, 0, len(candidates))
	for index := 0; index < len(contextual) || index < len(ordinary); index++ {
		if index < len(contextual) {
			ordered = append(ordered, contextual[index])
		}
		if index < len(ordinary) {
			ordered = append(ordered, ordinary[index])
		}
	}
	return ordered
}

func contentModerationContextualReviewFailureStatus(err error, attempted bool) string {
	if !attempted {
		return "not_attempted"
	}
	switch {
	case errors.Is(err, errContentModerationSecondLayerBusy):
		return "busy"
	case errors.Is(err, errContentModerationSecondLayerParse):
		return "parse_error"
	case isContentModerationSecondLayerTimeout(err):
		return "timeout"
	default:
		return "error"
	}
}

func (s *ContentModerationService) handleContextualReviewUnavailable(
	ctx context.Context,
	input ContentModerationCheckInput,
	cfg *ContentModerationConfig,
	namespace string,
	bundle contentModerationEvidenceBundle,
	primary ContentModerationFragment,
	whitelistShadow bool,
	parserStatus string,
	reviewErr error,
) *ContentModerationDecision {
	audit := unifiedModerationAudit{
		DecisionSource:    "review_unavailable",
		CacheNamespace:    namespace,
		EvidenceMode:      bundle.Evidence.Mode,
		EvidenceTruncated: bundle.Evidence.Truncated,
		ParserStatus:      parserStatus,
		KeywordTier:       "contextual_review",
		KeywordRuleID:     bundle.PrimaryRuleID,
		EvidenceText:      bundle.Evidence.Text,
		EvidenceWindows:   bundle.Windows,
		InputHash:         bundle.CacheHash,
	}
	if reviewErr != nil {
		audit.Error = trimRunes(redactContentModerationSecrets(reviewErr.Error()), maxModerationExcerptRunes)
	}
	if endpoints := cfg.enabledSecondLayerEndpoints(); len(endpoints) > 0 {
		audit.ModelProfile = endpoints[0].Profile
		audit.PromptVersion = endpoints[0].PromptVersion
	}
	if whitelistShadow {
		audit.DecisionSource = "review_unavailable_whitelist_shadow"
	} else if cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow {
		audit.DecisionSource = "review_unavailable_shadow"
	}
	s.persistUnifiedShadowAudit(ctx, input, cfg, primary, ContentModerationActionReviewUnavailable, "", bundle.PrimaryKeyword, audit)

	if whitelistShadow || cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow {
		return &ContentModerationDecision{Allowed: true, Action: ContentModerationActionAllow}
	}

	return &ContentModerationDecision{
		Allowed:        false,
		Blocked:        false,
		Flagged:        false,
		Message:        "Risk-control review is temporarily unavailable; retry later",
		StatusCode:     http.StatusServiceUnavailable,
		InputHash:      bundle.CacheHash,
		MatchedKeyword: bundle.PrimaryKeyword,
		Action:         ContentModerationActionReviewUnavailable,
		RetryAfter:     1,
	}
}

func (s *ContentModerationService) enqueueContentModerationShadowReview(job func()) bool {
	if s == nil || job == nil {
		return false
	}
	s.secondLayerShadowOnce.Do(func() {
		queue := make(chan func(), contentModerationShadowQueueCapacity)
		s.secondLayerShadowMu.Lock()
		s.secondLayerShadowQueue = queue
		s.secondLayerShadowMu.Unlock()
		go func() {
			for queued := range queue {
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							slog.Error("content_moderation.second_layer_shadow_panic", "panic", recovered)
						}
						s.secondLayerShadowDone.Add(1)
					}()
					queued()
				}()
			}
		}()
	})
	s.secondLayerShadowMu.RLock()
	queue := s.secondLayerShadowQueue
	s.secondLayerShadowMu.RUnlock()
	select {
	case queue <- job:
		s.secondLayerShadowQueued.Add(1)
		return true
	default:
		s.secondLayerShadowDropped.Add(1)
		return false
	}
}

func (s *ContentModerationService) contentModerationShadowQueueDepth() int {
	if s == nil {
		return 0
	}
	s.secondLayerShadowMu.RLock()
	defer s.secondLayerShadowMu.RUnlock()
	if s.secondLayerShadowQueue == nil {
		return 0
	}
	return len(s.secondLayerShadowQueue)
}

func contentModerationShadowReviewTimeout(cfg *ContentModerationConfig) time.Duration {
	timeout := 30 * time.Second
	if cfg != nil {
		for _, endpoint := range cfg.enabledSecondLayerEndpoints() {
			candidate := time.Duration(endpoint.TimeoutMS)*time.Millisecond + 5*time.Second
			if candidate > timeout {
				timeout = candidate
			}
		}
	}
	if timeout > 2*time.Minute {
		return 2 * time.Minute
	}
	return timeout
}

func (s *ContentModerationService) acquireContentModerationFragmentDecisionLock(key string) func() {
	if s == nil {
		return func() {}
	}
	s.fragmentDecisionMu.Lock()
	if s.fragmentDecisionLocks == nil {
		s.fragmentDecisionLocks = make(map[string]*contentModerationFragmentDecisionLock)
	}
	lock := s.fragmentDecisionLocks[key]
	if lock == nil {
		lock = &contentModerationFragmentDecisionLock{}
		s.fragmentDecisionLocks[key] = lock
	}
	lock.refs++
	s.fragmentDecisionMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.fragmentDecisionMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.fragmentDecisionLocks, key)
		}
		s.fragmentDecisionMu.Unlock()
	}
}

func (s *ContentModerationService) getUnifiedFragmentCache(ctx context.Context, cache ContentModerationFragmentCache, namespace, fragmentHash string) (ContentModerationFragmentCacheEntry, bool, error) {
	if ttlCache, ok := cache.(ContentModerationFragmentTTLCache); ok {
		return ttlCache.GetFragmentCacheEntry(ctx, namespace, fragmentHash)
	}
	result, found, err := cache.GetFragmentResult(ctx, namespace, fragmentHash)
	return ContentModerationFragmentCacheEntry{Result: result}, found, err
}

func (s *ContentModerationService) putUnifiedFragmentCache(ctx context.Context, cache ContentModerationFragmentCache, namespace string, cfg *ContentModerationConfig, fragment ContentModerationFragment, result string) {
	s.putUnifiedFragmentCacheEntry(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentCacheEntry{Result: result})
}

func (s *ContentModerationService) putUnifiedFragmentCacheEntry(ctx context.Context, cache ContentModerationFragmentCache, namespace string, cfg *ContentModerationConfig, fragment ContentModerationFragment, entry ContentModerationFragmentCacheEntry) {
	if cache == nil || cfg == nil {
		return
	}
	estimatedBytes := int64(len(fragment.Hash) + len(entry.Result) + len(namespace) + entryEstimatedBytes(entry) + 64)
	var err error
	if ttlCache, ok := cache.(ContentModerationFragmentTTLCache); ok {
		err = ttlCache.PutFragmentCacheEntry(ctx, namespace, fragment.Hash, entry, estimatedBytes, cfg.CacheMaxEntries, cfg.CacheMaxBytes, moderationFragmentTTL(cfg, entry.Result))
	} else {
		err = cache.PutFragmentResult(ctx, namespace, fragment.Hash, entry.Result, estimatedBytes, cfg.CacheMaxEntries, cfg.CacheMaxBytes)
	}
	if err != nil {
		s.fragmentCacheWriteErrors.Add(1)
		slog.Warn("content_moderation.fragment_cache_put_failed", "error", err)
	} else {
		s.fragmentCacheWrites.Add(1)
	}
}

func (s *ContentModerationService) unifiedBlockDecision(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, fragment ContentModerationFragment, action, category, keyword string) *ContentModerationDecision {
	decision, _, _ := s.unifiedBlockDecisionWithAudit(ctx, input, cfg, fragment, action, category, keyword, unifiedModerationAudit{})
	return decision
}

func (s *ContentModerationService) persistUnifiedShadowAudit(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, fragment ContentModerationFragment, action, category, keyword string, audit unifiedModerationAudit) {
	if strings.TrimSpace(action) == "" {
		action = ContentModerationActionSecondLayerShadow
	}
	highestScore := 0.0
	var scores map[string]float64
	if category != "" {
		highestScore = 1
		scores = map[string]float64{category: highestScore}
	}
	logInput := fragment.Text
	if strings.TrimSpace(audit.EvidenceText) != "" {
		logInput = audit.EvidenceText
	}
	log := s.buildLog(input, cfg, action, false, category, highestScore, scores, logInput, nil, nil, audit.Error)
	log.MatchedKeyword = keyword
	applyUnifiedModerationAudit(log, fragment, cfg, audit)
	inputHash := fragment.Hash
	if strings.TrimSpace(audit.InputHash) != "" {
		inputHash = audit.InputHash
		log.InputHash = inputHash
	}
	s.persistContentModerationLogWithInput(ctx, cfg, log, inputHash, false, false, &input)
}

type unifiedModerationAudit struct {
	CacheHit          bool
	DecisionSource    string
	SourceLogID       *int64
	ReplayOfInputHash string
	CacheNamespace    string
	ModelProfile      string
	PromptVersion     string
	EvidenceMode      string
	EvidenceTruncated bool
	ParserStatus      string
	KeywordTier       string
	KeywordRuleID     string
	EvidenceText      string
	EvidenceWindows   []ContentModerationEvidenceWindow
	InputHash         string
	Error             string
}

func (s *ContentModerationService) unifiedBlockDecisionWithAudit(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, fragment ContentModerationFragment, action, category, keyword string, audit unifiedModerationAudit) (*ContentModerationDecision, *ContentModerationLog, bool) {
	if category == "" {
		category = "content_policy"
	}
	scores := map[string]float64{category: 1}
	logInput := fragment.Text
	if strings.TrimSpace(audit.EvidenceText) != "" {
		logInput = audit.EvidenceText
	}
	log := s.buildLog(input, cfg, action, true, category, 1, scores, logInput, nil, nil, "")
	log.MatchedKeyword = keyword
	applyUnifiedModerationAudit(log, fragment, cfg, audit)
	applySideEffects := !audit.CacheHit && action != ContentModerationActionCacheBlock
	inputHash := fragment.Hash
	if strings.TrimSpace(audit.InputHash) != "" {
		inputHash = audit.InputHash
	}
	log.InputHash = inputHash
	persisted := s.persistContentModerationLogWithInput(ctx, cfg, log, inputHash, false, applySideEffects, &input)
	s.recordPreBlockSyncMetric(0, action)

	blocked := cfg.Mode == ContentModerationModePreBlock
	message := cfg.BlockMessage
	if message == "" {
		message = defaultContentModerationBlockMessage
	}
	return &ContentModerationDecision{
		Allowed:         !blocked,
		Blocked:         blocked,
		Flagged:         true,
		Message:         message,
		StatusCode:      cfg.BlockStatus,
		InputHash:       inputHash,
		MatchedKeyword:  keyword,
		HighestCategory: category,
		HighestScore:    1,
		CategoryScores:  scores,
		Action:          action,
	}, log, persisted
}

func applyUnifiedModerationAudit(log *ContentModerationLog, fragment ContentModerationFragment, cfg *ContentModerationConfig, audit unifiedModerationAudit) {
	if log == nil {
		return
	}
	log.CacheHit = audit.CacheHit
	log.DecisionSource = audit.DecisionSource
	log.SourceLogID = audit.SourceLogID
	log.ReplayOfInputHash = audit.ReplayOfInputHash
	log.FragmentRole = fragment.Role
	log.FragmentKind = fragment.Kind
	log.ContextClass = fragment.ContextClass
	log.FragmentPath = redactContentModerationPath(fragment.Path)
	log.CacheNamespace = audit.CacheNamespace
	log.ModelProfile = audit.ModelProfile
	log.PromptVersion = audit.PromptVersion
	log.EvidencePolicyVersion = cfg.EvidencePolicyVersion
	log.PolicyVersion = contentModerationPolicyDigest(cfg)
	log.KeywordTier = audit.KeywordTier
	log.KeywordRuleID = audit.KeywordRuleID
	log.EvidenceMode = audit.EvidenceMode
	log.EvidenceTruncated = audit.EvidenceTruncated
	log.ParserStatus = audit.ParserStatus
	log.EvidenceWindows = cloneContentModerationEvidenceWindows(audit.EvidenceWindows)
	if audit.CacheHit {
		log.ViolationCount = 0
		log.DispositionStatus = "not_counted"
	}
}

func cloneContentModerationEvidenceWindows(windows []ContentModerationEvidenceWindow) []ContentModerationEvidenceWindow {
	if len(windows) == 0 {
		return []ContentModerationEvidenceWindow{}
	}
	out := make([]ContentModerationEvidenceWindow, len(windows))
	for index, window := range windows {
		out[index] = window
		out[index].Matches = append([]ContentModerationEvidenceMatch(nil), window.Matches...)
	}
	return out
}

func contentModerationLogIDPtr(log *ContentModerationLog) *int64 {
	if log == nil || log.ID <= 0 {
		return nil
	}
	id := log.ID
	return &id
}

func entryEstimatedBytes(entry ContentModerationFragmentCacheEntry) int {
	return len(entry.ReplayOfInputHash) + len(entry.DecisionSource) + len(entry.Category) + len(entry.MatchedKeyword) +
		len(entry.ModelProfile) + len(entry.PromptVersion) + len(entry.EvidenceMode) + len(entry.ParserStatus) + 96
}

func contentModerationKeywordRuleID(keyword string) string {
	if strings.TrimSpace(keyword) == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(keyword))))
	return fmt.Sprintf("kw-%x", digest[:6])
}

func redactContentModerationPath(path string) string {
	path = strings.TrimSpace(path)
	if len(path) > 240 {
		path = path[:240]
	}
	return evidenceSecretPattern.ReplaceAllString(path, "$1$2[REDACTED]")
}

func (cfg *ContentModerationConfig) enabledSecondLayerEndpoints() []ContentModerationEndpoint {
	if cfg == nil || !cfg.SecondLayerEnabled {
		return nil
	}
	all := normalizeContentModerationEndpoints(cfg.SecondLayerEndpoints)
	out := make([]ContentModerationEndpoint, 0, len(all))
	for _, endpoint := range all {
		if endpoint.Enabled {
			out = append(out, endpoint)
		}
	}
	return out
}

func (s *ContentModerationService) validateUnifiedConfig(cfg *ContentModerationConfig) error {
	if cfg == nil {
		return nil
	}
	for _, endpoint := range cfg.SecondLayerEndpoints {
		if endpoint.ID == "" || endpoint.BaseURL == "" {
			return fmt.Errorf("second-layer endpoint id and base URL are required")
		}
		if endpoint.TimeoutMS > 0 && endpoint.TimeoutMS < minContentModerationSecondLayerTimeoutMS {
			return fmt.Errorf("second-layer endpoint timeout is below minimum")
		}
		if endpoint.InputLimit > 0 && endpoint.InputLimit < minContentModerationSecondLayerInputLimit {
			return fmt.Errorf("second-layer endpoint input limit is below minimum")
		}
		if _, err := normalizeContentModerationSecondLayerBaseURL(endpoint.BaseURL); err != nil {
			return err
		}
		if profile := strings.ToLower(strings.TrimSpace(endpoint.Profile)); profile != "" &&
			profile != ContentModerationModelProfileQwen && profile != "qwen" && profile != "qwen3guard" &&
			profile != ContentModerationModelProfileYuFengXGuard && profile != "yufeng" && profile != "yufeng-xguard" {
			return fmt.Errorf("unsupported second-layer model profile %q", endpoint.Profile)
		}
	}
	if cfg.FragmentBlockTTLSeconds != 0 && (cfg.FragmentBlockTTLSeconds < MinContentModerationFragmentBlockTTLSeconds || cfg.FragmentBlockTTLSeconds > MaxContentModerationFragmentBlockTTLSeconds) {
		return fmt.Errorf("fragment block TTL must be between %d and %d seconds", MinContentModerationFragmentBlockTTLSeconds, MaxContentModerationFragmentBlockTTLSeconds)
	}
	if cfg.FragmentAllowTTLSeconds < 0 || cfg.FragmentAllowTTLSeconds > MaxContentModerationFragmentAllowTTLSeconds {
		return fmt.Errorf("fragment allow TTL must be between 1 and %d seconds", MaxContentModerationFragmentAllowTTLSeconds)
	}
	if stage := strings.TrimSpace(cfg.FirstLayerStage); stage != "" && stage != ContentModerationFirstLayerStageEnforce && stage != ContentModerationFirstLayerStageShadow {
		return fmt.Errorf("unsupported first-layer stage %q", cfg.FirstLayerStage)
	}
	if stage := strings.TrimSpace(cfg.SecondLayerStage); stage != "" && stage != ContentModerationSecondLayerStageEnforce && stage != ContentModerationSecondLayerStageShadow {
		return fmt.Errorf("unsupported second-layer stage %q", cfg.SecondLayerStage)
	}
	if strings.TrimSpace(cfg.CacheVersion) == "" {
		return fmt.Errorf("content moderation cache version is required")
	}
	return nil
}
