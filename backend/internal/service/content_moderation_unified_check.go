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
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
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
	// Group and model scope is configuration-driven. The request snapshot keeps
	// the original API-key group stable across account/fallback routing, while
	// avoiding any provider- or name-based eligibility shortcut.
	if !cfg.includesGroup(input.Scope.GroupID) || !cfg.includesModel(input.Model) {
		return allow
	}
	whitelistShadow := cfg.includesUserEmail(input.UserEmail)

	fragments := SelectContentModerationReviewFragments(ExtractContentModerationFragments(input.Protocol, input.Body))
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
	// Model-review entries are shared across whitelist and ordinary traffic so
	// a later Enforce request can promote an already reviewed risk. Whole-
	// fragment decisions keep the whitelist suffix because their disposition
	// semantics differ; the hash domains also prevent cross-entry collisions.
	reviewNamespace := runtime.fragmentCacheNamespace
	if reviewNamespace == "" {
		reviewNamespace = cfg.fragmentCacheNamespace()
	}
	fragmentNamespace := reviewNamespace
	if whitelistShadow {
		fragmentNamespace += contentModerationWhitelistShadowCacheSuffix
	}
	candidates := make([]contentModerationCandidateFragment, 0, len(checkFragments))
	for _, checkFragment := range checkFragments {
		fragment := checkFragment.Fragment
		fragmentCacheEligible := checkFragment.CacheEligible
		shadowRiskObserved := false
		releaseDecisionLock := s.acquireContentModerationFragmentDecisionLock(fragmentNamespace + "\x00" + fragment.Hash)
		if cache != nil && fragmentCacheEligible {
			entry, found, err := s.getUnifiedFragmentCache(ctx, cache, fragmentNamespace, fragment.Hash)
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
				case ContentModerationFragmentBlock, ContentModerationFragmentRestricted:
					s.fragmentCacheReplays.Add(1)
					restrictedReplay := entry.Result == ContentModerationFragmentRestricted
					category := entry.Category
					keyword := entry.MatchedKeyword
					if category == "" && restrictedReplay {
						category = ContentModerationRestrictedCategory
					} else if category == "" && cfg.KeywordBlockingMode != ContentModerationKeywordModeAPIOnly && runtime.keywordMatcher != nil {
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
							Tier:     defaultContentModerationString(entry.KeywordTier, "high_confidence"),
						}}, contentModerationEvidenceWindowBudgetRunes, cfg)
					}
					evidenceMode := entry.EvidenceMode
					if cachedEvidence.Evidence.Mode != "" {
						evidenceMode = cachedEvidence.Evidence.Mode
					}
					evidenceTruncated := entry.EvidenceTruncated || cachedEvidence.Evidence.Truncated
					cachedAudit := unifiedModerationAudit{
						CacheHit: true, DecisionSource: "cache_replay", SourceLogID: entry.SourceLogID,
						ReplayOfInputHash: defaultContentModerationString(entry.ReplayOfInputHash, fragment.Hash), CacheNamespace: fragmentNamespace,
						ModelProfile: entry.ModelProfile, PromptVersion: entry.PromptVersion, EvidenceMode: evidenceMode,
						EvidenceTruncated: evidenceTruncated, ParserStatus: entry.ParserStatus,
						KeywordTier: entry.KeywordTier, KeywordRuleID: entry.KeywordRuleID,
						EvidenceText: cachedEvidence.Evidence.Text, EvidenceWindows: cachedEvidence.Windows,
					}
					if restrictedReplay {
						cachedAudit.ReviewOutcome = "policy_restricted"
						cachedAudit.DeepSeekCategory = ContentModerationRestrictedCategory
					}
					if whitelistShadow {
						shadowRiskObserved = true
						cachedAudit.DecisionSource = "cache_replay_whitelist_shadow"
						s.persistUnifiedShadowAudit(ctx, input, cfg, fragment, ContentModerationActionWhitelistShadow, category, keyword, cachedAudit)
						releaseDecisionLock()
						continue
					}
					var decision *ContentModerationDecision
					if restrictedReplay {
						decision, _, _ = s.unifiedRestrictedDecisionWithAudit(ctx, input, cfg, fragment, category, keyword, cachedAudit)
					} else {
						decision, _, _ = s.unifiedBlockDecisionWithAudit(ctx, input, cfg, fragment, ContentModerationActionCacheBlock, category, keyword, cachedAudit)
					}
					releaseDecisionLock()
					return decision
				}
			}
		}

		var contextualMatches []contentModerationKeywordMatch
		if cfg.KeywordBlockingMode != ContentModerationKeywordModeAPIOnly && runtime.keywordMatcher != nil {
			policyRestrictionContext := hasContentModerationPolicyRestrictionContext(fragment.Text)
			policyRestrictionDirect := false
			keyword, hardMatches, reviewMatches := classifyUnifiedHardKeywordMatches(fragment, runtime)
			if keyword == "" && len(reviewMatches) == 0 && checkFragment.WholeFragmentTruncated && runtime.contextualKeywordMatcher != nil {
				reviewMatches = runtime.contextualKeywordMatcher.MatchAll(fragment.Text)
			}
			if keyword == "" && len(reviewMatches) > 0 &&
				(!cfg.SecondLayerEnabled || cfg.KeywordBlockingMode == ContentModerationKeywordModeKeywordOnly) {
				keyword = reviewMatches[0].Keyword
				hardMatches = reviewMatches
				reviewMatches = nil
				policyRestrictionDirect = policyRestrictionContext
			}
			contextualMatches = reviewMatches
			if keyword != "" {
				keywordTier := "high_confidence"
				keywordCategory := contentModerationKeywordCategory
				if policyRestrictionDirect {
					keywordTier = contentModerationKeywordTierPolicyRestrictedReview
					keywordCategory = ContentModerationRestrictedCategory
				}
				hardEvidence := buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
					Fragment: fragment, Matches: hardMatches, Tier: keywordTier,
				}}, contentModerationEvidenceWindowBudgetRunes, cfg)
				audit := unifiedModerationAudit{
					DecisionSource: "keyword_high_confidence", CacheNamespace: fragmentNamespace, KeywordTier: keywordTier, KeywordRuleID: contentModerationKeywordRuleID(keyword),
					EvidenceMode: hardEvidence.Evidence.Mode, EvidenceTruncated: hardEvidence.Evidence.Truncated,
					EvidenceText: hardEvidence.Evidence.Text, EvidenceWindows: hardEvidence.Windows,
				}
				if policyRestrictionDirect {
					audit.DecisionSource = "policy_restriction_keyword"
					audit.ReviewOutcome = "policy_restricted"
					audit.DeepSeekCategory = ContentModerationRestrictedCategory
					audit.DeepSeekConfidence = 1
				}
				if whitelistShadow {
					shadowRiskObserved = true
					audit.DecisionSource = "keyword_high_confidence_whitelist_shadow"
					s.persistUnifiedShadowAudit(ctx, input, cfg, fragment, ContentModerationActionWhitelistShadow, keywordCategory, keyword, audit)
				} else if cfg.FirstLayerStage == ContentModerationFirstLayerStageShadow {
					shadowRiskObserved = true
					audit.DecisionSource = "keyword_high_confidence_shadow"
					audit.KeywordTier = "first_layer_shadow"
					s.persistUnifiedShadowAudit(ctx, input, cfg, fragment, ContentModerationActionFirstLayerShadow, keywordCategory, keyword, audit)
				} else {
					var decision *ContentModerationDecision
					var log *ContentModerationLog
					var persisted bool
					if policyRestrictionDirect {
						decision, log, persisted = s.unifiedRestrictedDecisionWithAudit(ctx, input, cfg, fragment, keywordCategory, keyword, audit)
					} else {
						decision, log, persisted = s.unifiedBlockDecisionWithAudit(ctx, input, cfg, fragment, ContentModerationActionKeywordBlock, keywordCategory, keyword, audit)
					}
					if persisted && fragmentCacheEligible {
						cacheResult := ContentModerationFragmentBlock
						if policyRestrictionDirect {
							cacheResult = ContentModerationFragmentRestricted
						}
						s.putUnifiedFragmentCacheEntry(ctx, cache, fragmentNamespace, cfg, fragment, ContentModerationFragmentCacheEntry{
							Result: cacheResult, SourceLogID: contentModerationLogIDPtr(log), ReplayOfInputHash: fragment.Hash,
							DecisionSource: "keyword_high_confidence", Category: keywordCategory, MatchedKeyword: keyword,
							KeywordTier: keywordTier, KeywordRuleID: contentModerationKeywordRuleID(keyword),
							EvidenceMode: hardEvidence.Evidence.Mode, EvidenceTruncated: hardEvidence.Evidence.Truncated,
						})
					}
					releaseDecisionLock()
					return decision
				}
			}
		}
		if len(contextualMatches) > 0 {
			// Contextual hard-keyword decisions retain the complete logical
			// fragment when it fits the reviewer budget. The builder distinguishes
			// harmless metadata bounds from omitted context that must fail closed.
			tier := contentModerationKeywordTierContextualReview
			if hasContentModerationPolicyRestrictionContext(fragment.Text) {
				tier = contentModerationKeywordTierPolicyRestrictedReview
			}
			candidates = appendOrMergeContentModerationCandidate(candidates, contentModerationCandidateFragment{
				Fragment: fragment, Matches: contextualMatches, Tier: tier,
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
					tier = contentModerationKeywordTierContextualReview
				}
				if hasContentModerationPolicyRestrictionContext(fragment.Text) {
					tier = contentModerationKeywordTierPolicyRestrictedReview
				}
				candidates = appendOrMergeContentModerationCandidate(candidates, contentModerationCandidateFragment{
					Fragment: fragment, Matches: candidateMatches, Tier: tier,
					WholeFragment:          checkFragment.WholeFragment || contentModerationPreserveWholeUserFragment(fragment),
					WholeFragmentTruncated: checkFragment.WholeFragmentTruncated,
				})
				releaseDecisionLock()
				continue
			}
			if len(contextualMatches) > 0 {
				releaseDecisionLock()
				continue
			}
			if fragmentCacheEligible && !whitelistShadow && !shadowRiskObserved {
				s.putUnifiedFragmentCache(ctx, cache, fragmentNamespace, cfg, fragment, ContentModerationFragmentAllow)
			}
			releaseDecisionLock()
			continue
		}
		if len(contextualMatches) > 0 {
			releaseDecisionLock()
			continue
		}

		if fragmentCacheEligible && !shadowRiskObserved {
			s.putUnifiedFragmentCache(ctx, cache, fragmentNamespace, cfg, fragment, ContentModerationFragmentAllow)
		}
		releaseDecisionLock()
	}
	if len(candidates) > 0 {
		return s.checkUnifiedCandidateEvidence(ctx, input, cfg, reviewNamespace, candidates, whitelistShadow)
	}
	return allow
}

func contentModerationPreserveWholeUserFragment(fragment ContentModerationFragment) bool {
	return fragment.ContextClass == ContentModerationContextUser &&
		utf8.RuneCountInString(fragment.Text) <= contentModerationEvidenceWindowBudgetRunes
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
	if hasContentModerationPolicyRestrictionContext(fragment.Text) {
		return "", nil, runtime.keywordMatcher.MatchAll(fragment.Text)
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
		leftLogicalFragments := make([]ContentModerationFragment, 0, len(left.members))
		for _, member := range left.members {
			leftLogicalFragments = append(leftLogicalFragments, fragments[member])
		}
		rightLogicalFragments := make([]ContentModerationFragment, 0, len(right.members))
		for _, member := range right.members {
			rightLogicalFragments = append(rightLogicalFragments, fragments[member])
		}
		leftFragment.Text = joinContentModerationFragmentsAtOriginalWhitespace(leftLogicalFragments)
		rightFragment.Text = joinContentModerationFragmentsAtOriginalWhitespace(rightLogicalFragments)
		boundaryTruncated := utf8.RuneCountInString(leftFragment.Text)+utf8.RuneCountInString(rightFragment.Text)+1 >
			contentModerationEvidenceWindowBudgetRunes
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
	contextBudget := contentModerationEvidenceWindowBudgetRunes - 1
	leftBudget := contextBudget / 2
	rightBudget := contextBudget - leftBudget
	leftRunes := utf8.RuneCountInString(left.Text)
	rightRunes := utf8.RuneCountInString(right.Text)
	if leftRunes < leftBudget {
		rightBudget += leftBudget - leftRunes
		leftBudget = leftRunes
	}
	if rightRunes < rightBudget {
		leftBudget += rightBudget - rightRunes
		rightBudget = rightRunes
	}
	leftContext := runeSuffix(left.Text, leftBudget)
	rightContext := runePrefix(right.Text, rightBudget)
	addIfTriggered := func(boundary, reviewText string, leftRunes, separatorRunes int) {
		matches := contentModerationScopeTriggerMatches(boundary, keywordMatcher, candidateMatcher, keywordAllowlist)
		crossingMatches := matches[:0]
		for _, match := range matches {
			if match.Start < leftRunes && match.End > leftRunes+separatorRunes {
				crossingMatches = append(crossingMatches, match)
			}
		}
		if len(crossingMatches) == 0 {
			return
		}
		triggerKeywords := make(map[string]struct{}, len(crossingMatches))
		for _, match := range crossingMatches {
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
		leftTextRunes := utf8.RuneCountInString(leftText)
		addIfTriggered(leftText+" "+rightText, leftContext+" "+rightContext, leftTextRunes, 1)
		addIfTriggered(leftText+rightText, leftContext+rightContext, leftTextRunes, 0)
	}
	if candidateMatcher != nil {
		limit := maxContentModerationBlockedKeywordRunes + 2
		leftText, _ := contentModerationPrefilterNormalizedSuffix(left.Text, limit)
		rightText := contentModerationPrefilterNormalizedPrefix(right.Text, limit)
		leftTextRunes := utf8.RuneCountInString(leftText)
		addIfTriggered(leftText+" "+rightText, leftContext+" "+rightContext, leftTextRunes, 1)
		addIfTriggered(leftText+rightText, leftContext+rightContext, leftTextRunes, 0)
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
			_ = combined.WriteByte(' ')
		}
		_, _ = combined.WriteString(fragment.Text)
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
	candidates []contentModerationCandidateFragment,
	whitelistShadow bool,
) *ContentModerationDecision {
	allow := &ContentModerationDecision{Allowed: true, Action: ContentModerationActionAllow}
	endpoints := cfg.enabledYuFengSecondLayerEndpoints()
	limit := contentModerationEvidenceWindowBudgetRunes
	if len(endpoints) > 0 {
		limit = endpoints[0].InputLimit
		for _, endpoint := range endpoints[1:] {
			if endpoint.InputLimit < limit {
				limit = endpoint.InputLimit
			}
		}
	}
	works := make([]contentModerationCandidateReviewWork, 0, len(candidates))
	for _, candidate := range candidates {
		bundle := buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{candidate}, limit, cfg)
		primary := candidate.Fragment
		primary.Text = bundle.Evidence.Text
		works = append(works, contentModerationCandidateReviewWork{
			bundle: bundle, primary: primary,
			reviewRequired:         isContentModerationContextualReviewTier(candidate.Tier) || candidate.WholeFragment,
			requireHealthyReviewer: !whitelistShadow && cfg.SecondLayerStage == ContentModerationSecondLayerStageEnforce,
		})
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
		asyncCfg := cloneContentModerationConfig(cfg)
		for index := range works {
			work := works[index]
			s.launchContentModerationShadowReview(func() {
				timeout := contentModerationShadowReviewTimeout(asyncCfg)
				shadowCtx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				outcome := s.reviewUnifiedCandidateEvidenceBundle(shadowCtx, asyncCfg, namespace, work)
				if outcome.err != nil {
					_ = s.handleContextualReviewUnavailable(
						shadowCtx, asyncInput, asyncCfg, namespace, work.bundle, work.primary, whitelistShadow,
						outcome.parserStatus, outcome.err, outcome.result,
					)
					return
				}
				_ = s.applyUnifiedCandidateReviewResult(
					shadowCtx, asyncInput, asyncCfg, namespace, work, outcome, whitelistShadow, false,
				)
			})
		}
		return allow
	}

	outcomes := make([]contentModerationCandidateReviewOutcome, len(works))
	var wait sync.WaitGroup
	wait.Add(len(works))
	for index := range works {
		index := index
		go func() {
			defer wait.Done()
			outcomes[index] = s.reviewUnifiedCandidateEvidenceBundle(ctx, cfg, namespace, works[index])
		}()
	}
	wait.Wait()

	hasFailure := false
	for _, outcome := range outcomes {
		if outcome.err != nil {
			hasFailure = true
			break
		}
	}
	var unavailableDecision *ContentModerationDecision
	var blockDecision *ContentModerationDecision
	formalBlockIndex := -1
	for index, outcome := range outcomes {
		if outcome.err == nil && outcome.result.normalizedDisposition() == ContentModerationReviewDispositionViolation {
			formalBlockIndex = index
			break
		}
	}
	if formalBlockIndex < 0 {
		for index, outcome := range outcomes {
			if outcome.err == nil && outcome.result.Blocked {
				formalBlockIndex = index
				break
			}
		}
	}
	for index, outcome := range outcomes {
		work := works[index]
		if outcome.err != nil {
			decision := s.handleContextualReviewUnavailable(
				ctx, input, cfg, namespace, work.bundle, work.primary, whitelistShadow,
				outcome.parserStatus, outcome.err, outcome.result,
			)
			if unavailableDecision == nil {
				unavailableDecision = decision
			}
			continue
		}
		forceShadow := hasFailure || (outcome.result.Blocked && index != formalBlockIndex)
		decision := s.applyUnifiedCandidateReviewResult(
			ctx, input, cfg, namespace, work, outcome, whitelistShadow, forceShadow,
		)
		if outcome.result.Blocked && !forceShadow {
			blockDecision = decision
		}
	}
	if unavailableDecision != nil {
		return unavailableDecision
	}
	if blockDecision != nil {
		return blockDecision
	}
	return allow
}

type contentModerationCandidateReviewWork struct {
	bundle                 contentModerationEvidenceBundle
	primary                ContentModerationFragment
	reviewRequired         bool
	requireHealthyReviewer bool
}

type contentModerationCandidateReviewOutcome struct {
	result             contentModerationSecondLayerResult
	parserStatus       string
	cacheHit           bool
	cachePromotion     bool
	dispositionApplied bool
	coalesced          bool
	sourceLogID        *int64
	replayHash         string
	err                error
}

const (
	contentModerationSecondLayerReviewCacheVersion = 4
	contentModerationAuditPersistenceTimeout       = 10 * time.Second
)

type contentModerationCandidateReviewFlight struct {
	done    chan struct{}
	outcome contentModerationCandidateReviewOutcome
}

func (s *ContentModerationService) reviewUnifiedCandidateEvidenceBundle(
	ctx context.Context,
	cfg *ContentModerationConfig,
	namespace string,
	work contentModerationCandidateReviewWork,
) contentModerationCandidateReviewOutcome {
	if strings.TrimSpace(work.bundle.Evidence.Text) == "" || work.bundle.CacheHash == "" {
		slog.Warn("content_moderation.candidate_evidence_empty")
		return contentModerationCandidateReviewOutcome{
			parserStatus: "not_attempted", err: errors.New("layer 2 candidate evidence is empty"),
		}
	}
	if !contentModerationRemoteReviewersEnabled(cfg) && (!cfg.YuFengEnabled || len(cfg.enabledYuFengSecondLayerEndpoints()) == 0) {
		return contentModerationCandidateReviewOutcome{
			parserStatus: "not_attempted", err: errors.New("layer 2 reviewer is unavailable"),
		}
	}
	flightKey := namespace + "\x00layer2-review\x00" + work.bundle.CacheHash
	if work.bundle.ContextIncomplete || work.bundle.CoverageIncomplete {
		// An incomplete request can share the same bounded evidence hash with a
		// complete request. Keep their in-flight reviews separate so a complete
		// allow verdict cannot bypass the fail-closed evidence gate below.
		flightKey += "\x00incomplete\x00" + strconv.FormatBool(work.bundle.ContextIncomplete) +
			"\x00" + strconv.FormatBool(work.bundle.CoverageIncomplete) + "\x00" + work.primary.Hash
	}
	flight, leader := s.beginContentModerationCandidateReviewFlight(flightKey)
	if leader {
		reviewCfg := cloneContentModerationConfig(cfg)
		go func() {
			outcome := contentModerationCandidateReviewOutcome{}
			defer func() {
				if recovered := recover(); recovered != nil {
					slog.Error("content_moderation.layer2_review_panic", "panic", recovered)
					outcome = contentModerationCandidateReviewOutcome{
						parserStatus: "error", err: fmt.Errorf("layer 2 review panic: %v", recovered),
					}
				}
				s.finishContentModerationCandidateReviewFlight(flightKey, flight, outcome)
			}()
			timeout := contentModerationShadowReviewTimeout(reviewCfg)
			reviewCtx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			outcome = s.reviewUnifiedCandidateEvidenceBundleUncached(reviewCtx, reviewCfg, namespace, work)
		}()
	}
	select {
	case <-ctx.Done():
		return contentModerationCandidateReviewOutcome{
			parserStatus: contentModerationContextualReviewFailureStatus(ctx.Err(), true), err: ctx.Err(),
		}
	case <-flight.done:
		outcome := flight.outcome
		if !leader && outcome.err == nil && !outcome.cacheHit {
			outcome.coalesced = true
		}
		return outcome
	}
}

func (s *ContentModerationService) reviewUnifiedCandidateEvidenceBundleUncached(
	ctx context.Context,
	cfg *ContentModerationConfig,
	namespace string,
	work contentModerationCandidateReviewWork,
) contentModerationCandidateReviewOutcome {
	if !work.bundle.ContextIncomplete && !work.bundle.CoverageIncomplete {
		if entry, found := s.getUnifiedCandidateReviewCache(ctx, namespace, work.bundle.CacheHash); found {
			return contentModerationCandidateReviewOutcome{
				result:             resultFromUnifiedCandidateReviewCache(entry),
				cacheHit:           true,
				dispositionApplied: entry.DispositionApplied,
				sourceLogID:        entry.SourceLogID,
				replayHash:         defaultContentModerationString(entry.ReplayOfInputHash, work.bundle.CacheHash),
			}
		}
	}
	reviewCtx := ctx
	cancelReview := func() {}
	if work.requireHealthyReviewer && contentModerationRemoteReviewersEnabled(cfg) {
		totalTimeout := cfg.DeepSeekTotalTimeoutMS
		if totalTimeout <= 0 {
			totalTimeout = DefaultContentModerationDeepSeekTotalTimeoutMS
		}
		reviewCtx, cancelReview = context.WithTimeout(ctx, time.Duration(totalTimeout)*time.Millisecond)
	}
	defer cancelReview()
	if work.requireHealthyReviewer {
		if ready, reason := s.ensureContentModerationSecondLayerEnforceReadiness(reviewCtx, cfg, time.Now()); !ready {
			return contentModerationCandidateReviewOutcome{
				parserStatus: "health_not_ready", err: errors.New(reason),
			}
		}
	}
	reviewCfg := cfg
	if work.requireHealthyReviewer && contentModerationRemoteReviewersEnabled(cfg) {
		reviewCfg = s.contentModerationConfigWithReachableDeepSeekFirst(cfg, time.Now())
	}
	result, attempted, err := s.scanUnifiedSecondLayerPrepared(reviewCtx, reviewCfg, contentModerationSecondLayerInput{
		Fragment: work.bundle.Fragment, Evidence: work.bundle.Evidence,
		KeywordTier: defaultContentModerationString(work.bundle.PrimaryTier, "candidate"), KeywordRuleID: work.bundle.PrimaryRuleID,
		Background: cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow,
	})
	s.recordContentModerationReviewAttempts(result.ReviewAttempts, err)
	if err != nil {
		logger.FromContext(reviewCtx).Warn("content_moderation.second_layer_failed",
			zap.String("component", "service.content_moderation"),
			zap.String("failure_class", contentModerationReviewFailureClass(err, result.ReviewAttempts)),
			zap.Int("review_attempt_count", len(result.ReviewAttempts)),
		)
		return contentModerationCandidateReviewOutcome{
			result: result, parserStatus: contentModerationContextualReviewFailureStatus(err, true), err: err,
		}
	}
	if !attempted {
		return contentModerationCandidateReviewOutcome{
			result: result, parserStatus: "not_attempted", err: errors.New("layer 2 review was not attempted"),
		}
	}
	result = applyContentModerationPolicyRestrictionFloor(work.bundle.PrimaryTier, result)
	if work.reviewRequired && !result.Blocked && (work.bundle.CoverageIncomplete || work.bundle.ContextIncomplete) {
		return contentModerationCandidateReviewOutcome{
			result: result, parserStatus: "evidence_truncated", err: errors.New("contextual review evidence coverage was incomplete"),
		}
	}
	return contentModerationCandidateReviewOutcome{result: result}
}

func (s *ContentModerationService) beginContentModerationCandidateReviewFlight(key string) (*contentModerationCandidateReviewFlight, bool) {
	s.secondLayerReviewMu.Lock()
	defer s.secondLayerReviewMu.Unlock()
	if s.secondLayerReviewFlights == nil {
		s.secondLayerReviewFlights = make(map[string]*contentModerationCandidateReviewFlight)
	}
	if flight := s.secondLayerReviewFlights[key]; flight != nil {
		return flight, false
	}
	flight := &contentModerationCandidateReviewFlight{done: make(chan struct{})}
	s.secondLayerReviewFlights[key] = flight
	return flight, true
}

func (s *ContentModerationService) finishContentModerationCandidateReviewFlight(
	key string,
	flight *contentModerationCandidateReviewFlight,
	outcome contentModerationCandidateReviewOutcome,
) {
	s.secondLayerReviewMu.Lock()
	flight.outcome = outcome
	delete(s.secondLayerReviewFlights, key)
	close(flight.done)
	s.secondLayerReviewMu.Unlock()
}

func (s *ContentModerationService) getUnifiedCandidateReviewCache(
	ctx context.Context,
	namespace string,
	evidenceHash string,
) (ContentModerationFragmentCacheEntry, bool) {
	return s.getUnifiedCandidateReviewCacheForApply(ctx, namespace, evidenceHash, true, true)
}

func (s *ContentModerationService) getUnifiedCandidateReviewCacheForApply(
	ctx context.Context,
	namespace string,
	evidenceHash string,
	recordMiss bool,
	recordHit bool,
) (ContentModerationFragmentCacheEntry, bool) {
	cache, ok := s.hashCache.(ContentModerationFragmentTTLCache)
	if !ok || strings.TrimSpace(namespace) == "" || strings.TrimSpace(evidenceHash) == "" {
		return ContentModerationFragmentCacheEntry{}, false
	}
	entry, found, err := cache.GetFragmentCacheEntry(ctx, namespace, evidenceHash)
	if err != nil {
		s.fragmentCacheErrors.Add(1)
		s.secondLayerCacheErrors.Add(1)
		slog.Warn("content_moderation.layer2_cache_get_failed", "error", err)
		return ContentModerationFragmentCacheEntry{}, false
	}
	if !found {
		if entry.Expired {
			s.fragmentCacheExpired.Add(1)
		}
		if recordMiss {
			s.fragmentCacheMisses.Add(1)
			s.secondLayerCacheMisses.Add(1)
		}
		return ContentModerationFragmentCacheEntry{}, false
	}
	if entry.ReviewCacheVersion != contentModerationSecondLayerReviewCacheVersion ||
		entry.DecisionSource != "model" || !contentModerationParserStatusCacheable(entry.ParserStatus) ||
		(entry.Result != ContentModerationFragmentAllow && entry.Result != ContentModerationFragmentBlock &&
			entry.Result != ContentModerationFragmentRestricted) {
		if recordMiss {
			s.fragmentCacheMisses.Add(1)
			s.secondLayerCacheMisses.Add(1)
		}
		return ContentModerationFragmentCacheEntry{}, false
	}
	if recordHit {
		s.fragmentCacheHits.Add(1)
		s.fragmentCacheReplays.Add(1)
		s.secondLayerCacheHits.Add(1)
	}
	return entry, true
}

func contentModerationParserStatusCacheable(status string) bool {
	switch strings.TrimSpace(status) {
	case "parsed", contentModerationParserStatusNormalizedAllowConfidence:
		return true
	default:
		return false
	}
}

func (s *ContentModerationService) putUnifiedCandidateReviewCache(
	ctx context.Context,
	namespace string,
	cfg *ContentModerationConfig,
	evidenceHash string,
	result contentModerationSecondLayerResult,
	sourceLogID *int64,
	dispositionApplied bool,
) {
	cache, ok := s.hashCache.(ContentModerationFragmentTTLCache)
	if !ok {
		return
	}
	cacheResult := ContentModerationFragmentAllow
	switch result.normalizedDisposition() {
	case ContentModerationReviewDispositionViolation:
		cacheResult = ContentModerationFragmentBlock
	case ContentModerationReviewDispositionRestricted:
		cacheResult = ContentModerationFragmentRestricted
	}
	reviewOutcome := "safe"
	switch {
	case result.ConsensusStatus == "confirmed_violation":
		reviewOutcome = "confirmed_violation"
	case result.ConsensusStatus == "confirmed_restricted":
		reviewOutcome = "policy_restricted"
	case result.ConsensusStatus == "disagreement_restricted":
		reviewOutcome = "disagreement_restricted"
	case result.ConsensusStatus == "consensus_unavailable":
		reviewOutcome = "unavailable"
	case result.ReviewerMismatch:
		reviewOutcome = "disagreement"
	case result.ConsensusStatus == "local_shadow":
		reviewOutcome = "local_shadow"
	case result.normalizedDisposition() == ContentModerationReviewDispositionRestricted:
		reviewOutcome = "policy_restricted"
	case result.Blocked:
		reviewOutcome = "risky"
	case result.Confidence >= 0.50:
		reviewOutcome = "uncertain"
	}
	entry := ContentModerationFragmentCacheEntry{
		Result: cacheResult, SourceLogID: sourceLogID, ReplayOfInputHash: evidenceHash, DecisionSource: "model",
		Category: result.Category, ModelProfile: result.Profile, PromptVersion: result.PromptVersion,
		KeywordTier: result.KeywordTier, KeywordRuleID: result.KeywordRuleID,
		EvidenceMode: result.EvidenceMode, EvidenceTruncated: result.EvidenceTruncated,
		ParserStatus: result.ParserStatus, ReviewCacheVersion: contentModerationSecondLayerReviewCacheVersion,
		DispositionApplied: dispositionApplied,
		Confidence:         result.Confidence, Reason: result.Reason, Label: result.Label, EndpointID: result.EndpointID,
		ReviewOutcome: reviewOutcome, ReviewerDisagreement: result.ReviewerMismatch,
		ConsensusStatus: result.ConsensusStatus, RemoteVotes: result.RemoteVotes,
		ReviewAttempts: append([]ContentModerationReviewAttempt(nil), result.ReviewAttempts...),
	}
	estimatedBytes := int64(len(evidenceHash) + len(cacheResult) + len(namespace) + entryEstimatedBytes(entry) + 64)
	err := cache.PutFragmentCacheEntry(
		ctx, namespace, evidenceHash, entry, estimatedBytes, cfg.CacheMaxEntries, cfg.CacheMaxBytes,
		moderationFragmentTTL(cfg, cacheResult),
	)
	if err != nil {
		s.fragmentCacheWriteErrors.Add(1)
		s.secondLayerCacheErrors.Add(1)
		slog.Warn("content_moderation.layer2_cache_put_failed", "error", err)
		return
	}
	s.fragmentCacheWrites.Add(1)
	s.secondLayerCacheWrites.Add(1)
}

func resultFromUnifiedCandidateReviewCache(entry ContentModerationFragmentCacheEntry) contentModerationSecondLayerResult {
	result := contentModerationSecondLayerResult{
		Blocked: entry.Result != ContentModerationFragmentAllow, Category: entry.Category,
		Confidence: entry.Confidence, Reason: entry.Reason, Label: entry.Label,
		Profile: entry.ModelProfile, PromptVersion: entry.PromptVersion, ParserStatus: entry.ParserStatus,
		EvidenceMode: entry.EvidenceMode, EvidenceTruncated: entry.EvidenceTruncated,
		EndpointID: entry.EndpointID, KeywordTier: entry.KeywordTier, KeywordRuleID: entry.KeywordRuleID,
		ReviewAttempts:   append([]ContentModerationReviewAttempt(nil), entry.ReviewAttempts...),
		ReviewerMismatch: entry.ReviewerDisagreement,
		ConsensusStatus:  entry.ConsensusStatus, RemoteVotes: entry.RemoteVotes,
	}
	switch entry.Result {
	case ContentModerationFragmentRestricted:
		result.setDisposition(ContentModerationReviewDispositionRestricted)
	case ContentModerationFragmentBlock:
		result.setDisposition(ContentModerationReviewDispositionViolation)
	default:
		result.setDisposition(ContentModerationReviewDispositionAllow)
	}
	return applyContentModerationPolicyRestrictionFloor(entry.KeywordTier, result)
}

func outcomeFromUnifiedCandidateReviewCache(entry ContentModerationFragmentCacheEntry, evidenceHash string) contentModerationCandidateReviewOutcome {
	return contentModerationCandidateReviewOutcome{
		result:             resultFromUnifiedCandidateReviewCache(entry),
		cacheHit:           true,
		dispositionApplied: entry.DispositionApplied,
		sourceLogID:        entry.SourceLogID,
		replayHash:         defaultContentModerationString(entry.ReplayOfInputHash, evidenceHash),
		parserStatus:       entry.ParserStatus,
	}
}

func contentModerationCandidateNeedsDisposition(
	outcome contentModerationCandidateReviewOutcome,
	cfg *ContentModerationConfig,
	whitelistShadow bool,
	forceShadow bool,
) bool {
	return outcome.result.Blocked && !outcome.dispositionApplied &&
		cfg != nil && cfg.SecondLayerStage == ContentModerationSecondLayerStageEnforce &&
		!whitelistShadow && !forceShadow
}

func (s *ContentModerationService) applyUnifiedCandidateReviewResult(
	ctx context.Context,
	input ContentModerationCheckInput,
	cfg *ContentModerationConfig,
	namespace string,
	work contentModerationCandidateReviewWork,
	outcome contentModerationCandidateReviewOutcome,
	whitelistShadow bool,
	forceShadow bool,
) *ContentModerationDecision {
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), contentModerationAuditPersistenceTimeout)
	defer cancelPersist()

	cacheEligible := !work.bundle.ContextIncomplete && !work.bundle.CoverageIncomplete
	var releaseCommit func()
	needsDisposition := contentModerationCandidateNeedsDisposition(outcome, cfg, whitelistShadow, forceShadow)
	if cacheEligible && (!outcome.cacheHit || needsDisposition) {
		startedFromCache := outcome.cacheHit
		releaseCommit = s.acquireContentModerationFragmentDecisionLock(namespace + "\x00layer2-review-commit\x00" + work.bundle.CacheHash)
		if entry, found := s.getUnifiedCandidateReviewCacheForApply(
			persistCtx, namespace, work.bundle.CacheHash, false, !startedFromCache,
		); found {
			outcome = outcomeFromUnifiedCandidateReviewCache(entry, work.bundle.CacheHash)
			if contentModerationCandidateNeedsDisposition(outcome, cfg, whitelistShadow, forceShadow) {
				outcome.cacheHit = false
				outcome.cachePromotion = true
			} else {
				releaseCommit()
				releaseCommit = nil
			}
		} else if startedFromCache {
			// The entry expired or was cleared between review and commit. The
			// already parsed result remains usable, but must own disposition and
			// republish the cache before any later request can replay it.
			outcome.cacheHit = false
			outcome.cachePromotion = true
		}
	}
	if releaseCommit != nil {
		defer releaseCommit()
	}

	allow := &ContentModerationDecision{Allowed: true, Action: ContentModerationActionAllow}
	bundle := work.bundle
	primary := work.primary
	result := outcome.result
	publishCache := func(log *ContentModerationLog, persisted bool) {
		if outcome.cacheHit || !cacheEligible || !persisted || log == nil {
			return
		}
		dispositionApplied := !result.Blocked ||
			(cfg.SecondLayerStage == ContentModerationSecondLayerStageEnforce && !whitelistShadow && !forceShadow)
		s.putUnifiedCandidateReviewCache(
			persistCtx, namespace, cfg, bundle.CacheHash, result, contentModerationLogIDPtr(log), dispositionApplied,
		)
	}
	decisionSource := "model"
	if outcome.cachePromotion {
		decisionSource = "cache_promotion"
	} else if outcome.coalesced {
		decisionSource = "model_coalesced"
	}
	reviewAttempts := append([]ContentModerationReviewAttempt(nil), result.ReviewAttempts...)
	if outcome.cacheHit || outcome.cachePromotion {
		reviewAttempts = nil
	}
	audit := unifiedModerationAudit{
		CacheHit: outcome.cacheHit, DecisionSource: decisionSource, SourceLogID: outcome.sourceLogID,
		ReplayOfInputHash: outcome.replayHash, CacheNamespace: namespace, ModelProfile: result.Profile,
		PromptVersion: result.PromptVersion, EvidenceMode: result.EvidenceMode,
		EvidenceTruncated: result.EvidenceTruncated, ParserStatus: result.ParserStatus,
		KeywordTier: result.KeywordTier, KeywordRuleID: result.KeywordRuleID,
		EvidenceText: bundle.Evidence.Text, EvidenceWindows: bundle.Windows, InputHash: bundle.CacheHash,
		DeepSeekConfidence: result.Confidence, DeepSeekCategory: result.Category, DeepSeekReason: result.Reason,
		ReviewerDisagreement: result.ReviewerMismatch, ReviewAttempts: reviewAttempts,
		ConsensusStatus: result.ConsensusStatus, RemoteVotes: result.RemoteVotes,
	}
	switch {
	case result.ConsensusStatus == "confirmed_violation":
		audit.ReviewOutcome = "confirmed_violation"
	case result.ConsensusStatus == "confirmed_restricted":
		audit.ReviewOutcome = "policy_restricted"
	case result.ConsensusStatus == "disagreement_restricted":
		audit.ReviewOutcome = "disagreement_restricted"
	case result.ConsensusStatus == "consensus_unavailable":
		audit.ReviewOutcome = "unavailable"
	case result.ReviewerMismatch:
		audit.ReviewOutcome = "disagreement"
	case result.ConsensusStatus == "local_shadow":
		audit.ReviewOutcome = "local_shadow"
	case result.normalizedDisposition() == ContentModerationReviewDispositionRestricted:
		audit.ReviewOutcome = "policy_restricted"
	case result.Blocked:
		audit.ReviewOutcome = "risky"
	case result.Confidence >= 0.50:
		audit.ReviewOutcome = "uncertain"
	default:
		audit.ReviewOutcome = "safe"
	}
	if result.Blocked {
		if whitelistShadow || cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow || forceShadow {
			action := ContentModerationActionSecondLayerShadow
			audit.DecisionSource = "model_shadow"
			if whitelistShadow {
				action = ContentModerationActionWhitelistShadow
				audit.DecisionSource = "model_whitelist_shadow"
			} else if forceShadow && cfg.SecondLayerStage == ContentModerationSecondLayerStageEnforce {
				audit.DecisionSource = "model_enforce_suppressed"
			}
			log, persisted := s.persistUnifiedShadowAudit(persistCtx, input, cfg, primary, action, result.Category, bundle.PrimaryKeyword, audit)
			publishCache(log, persisted)
			return allow
		}
		var decision *ContentModerationDecision
		var log *ContentModerationLog
		var persisted bool
		if result.normalizedDisposition() == ContentModerationReviewDispositionRestricted {
			decision, log, persisted = s.unifiedRestrictedDecisionWithAudit(
				persistCtx, input, cfg, primary, result.Category, bundle.PrimaryKeyword, audit,
			)
		} else {
			decision, log, persisted = s.unifiedBlockDecisionWithAudit(
				persistCtx, input, cfg, primary, ContentModerationActionSecondLayerBlock,
				result.Category, bundle.PrimaryKeyword, audit,
			)
		}
		publishCache(log, persisted)
		return decision
	}

	action := ContentModerationActionAllow
	if cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow {
		action = ContentModerationActionSecondLayerShadow
		audit.DecisionSource = "model_shadow"
	} else if forceShadow {
		action = ContentModerationActionSecondLayerShadow
		audit.DecisionSource = "model_enforce_suppressed"
	}
	log := s.buildLog(input, cfg, action, false, "", 0, nil, bundle.Evidence.Text, nil, nil, "")
	log.MatchedKeyword = bundle.PrimaryKeyword
	applyUnifiedModerationAudit(log, primary, cfg, audit)
	persisted := s.persistContentModerationLogWithInput(persistCtx, cfg, log, bundle.CacheHash, false, false, &input)
	publishCache(log, persisted)
	return allow
}

func interleaveContextualReviewCandidates(candidates []contentModerationCandidateFragment) []contentModerationCandidateFragment {
	contextual := make([]contentModerationCandidateFragment, 0, len(candidates))
	ordinary := make([]contentModerationCandidateFragment, 0, len(candidates))
	for _, candidate := range candidates {
		if isContentModerationContextualReviewTier(candidate.Tier) {
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
	reviewResults ...contentModerationSecondLayerResult,
) *ContentModerationDecision {
	persistCtx, cancelPersist := context.WithTimeout(context.WithoutCancel(ctx), contentModerationAuditPersistenceTimeout)
	defer cancelPersist()

	audit := unifiedModerationAudit{
		DecisionSource:    "review_unavailable",
		CacheNamespace:    namespace,
		EvidenceMode:      bundle.Evidence.Mode,
		EvidenceTruncated: bundle.Evidence.Truncated,
		ParserStatus:      parserStatus,
		KeywordTier:       defaultContentModerationString(bundle.PrimaryTier, "candidate"),
		KeywordRuleID:     bundle.PrimaryRuleID,
		EvidenceText:      bundle.Evidence.Text,
		EvidenceWindows:   bundle.Windows,
		InputHash:         bundle.CacheHash,
		ReviewOutcome:     "unavailable",
	}
	if len(reviewResults) > 0 {
		result := reviewResults[0]
		audit.ModelProfile = result.Profile
		audit.PromptVersion = result.PromptVersion
		audit.DeepSeekConfidence = result.Confidence
		audit.DeepSeekCategory = result.Category
		audit.DeepSeekReason = result.Reason
		audit.ReviewerDisagreement = result.ReviewerMismatch
		audit.ConsensusStatus = result.ConsensusStatus
		audit.RemoteVotes = result.RemoteVotes
		audit.ReviewAttempts = append([]ContentModerationReviewAttempt(nil), result.ReviewAttempts...)
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
	s.recordContentModerationReviewUnavailable(ctx, input.RequestID, cfg, audit.DecisionSource, parserStatus, reviewErr, audit.ReviewAttempts)
	if bundle.PrimaryTier == contentModerationKeywordTierPolicyRestrictedReview &&
		!whitelistShadow && cfg.SecondLayerStage != ContentModerationSecondLayerStageShadow {
		audit.DecisionSource = "policy_floor_review_unavailable"
		audit.ReviewOutcome = "policy_restricted"
		decision, _, _ := s.unifiedRestrictedDecisionWithAudit(
			persistCtx, input, cfg, primary, ContentModerationRestrictedCategory, bundle.PrimaryKeyword, audit,
		)
		return decision
	}
	s.persistUnifiedShadowAudit(persistCtx, input, cfg, primary, ContentModerationActionReviewUnavailable, "", bundle.PrimaryKeyword, audit)

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

func (s *ContentModerationService) launchContentModerationShadowReview(job func()) bool {
	if s == nil || job == nil {
		return false
	}
	s.secondLayerShadowSubmitted.Add(1)
	s.secondLayerShadowInFlight.Add(1)
	go func() {
		defer func() {
			s.secondLayerShadowInFlight.Add(-1)
			if recovered := recover(); recovered != nil {
				slog.Error("content_moderation.second_layer_shadow_panic", "panic", recovered)
			}
			s.secondLayerShadowCompleted.Add(1)
		}()
		job()
	}()
	return true
}

func contentModerationShadowReviewTimeout(cfg *ContentModerationConfig) time.Duration {
	timeout := 30 * time.Second
	if cfg != nil {
		for _, endpoint := range cfg.enabledSecondLayerEndpoints() {
			candidate := 2*time.Duration(endpoint.TimeoutMS)*time.Millisecond + 5*time.Second
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

func (s *ContentModerationService) persistUnifiedShadowAudit(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, fragment ContentModerationFragment, action, category, keyword string, audit unifiedModerationAudit) (*ContentModerationLog, bool) {
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
	persisted := s.persistContentModerationLogWithInput(ctx, cfg, log, inputHash, false, false, &input)
	return log, persisted
}

type unifiedModerationAudit struct {
	CacheHit             bool
	DecisionSource       string
	SourceLogID          *int64
	ReplayOfInputHash    string
	CacheNamespace       string
	ModelProfile         string
	PromptVersion        string
	EvidenceMode         string
	EvidenceTruncated    bool
	ParserStatus         string
	KeywordTier          string
	KeywordRuleID        string
	EvidenceText         string
	EvidenceWindows      []ContentModerationEvidenceWindow
	InputHash            string
	Error                string
	DeepSeekConfidence   float64
	DeepSeekCategory     string
	DeepSeekReason       string
	ReviewOutcome        string
	ReviewerDisagreement bool
	ConsensusStatus      string
	RemoteVotes          int
	ReviewAttempts       []ContentModerationReviewAttempt
}

func (s *ContentModerationService) unifiedBlockDecisionWithAudit(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, fragment ContentModerationFragment, action, category, keyword string, audit unifiedModerationAudit) (*ContentModerationDecision, *ContentModerationLog, bool) {
	return s.unifiedBlockingDecisionWithAudit(ctx, input, cfg, fragment, action, category, keyword, audit, true)
}

func (s *ContentModerationService) unifiedRestrictedDecisionWithAudit(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, fragment ContentModerationFragment, category, keyword string, audit unifiedModerationAudit) (*ContentModerationDecision, *ContentModerationLog, bool) {
	return s.unifiedBlockingDecisionWithAudit(
		ctx, input, cfg, fragment, ContentModerationActionRestrictedBlock, category, keyword, audit, false,
	)
}

func (s *ContentModerationService) unifiedBlockingDecisionWithAudit(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, fragment ContentModerationFragment, action, category, keyword string, audit unifiedModerationAudit, flagged bool) (*ContentModerationDecision, *ContentModerationLog, bool) {
	if category == "" {
		if flagged {
			category = "content_policy"
		} else {
			category = ContentModerationRestrictedCategory
		}
	}
	score := audit.DeepSeekConfidence
	if score <= 0 {
		score = 1
	}
	scores := map[string]float64{category: score}
	logInput := fragment.Text
	if strings.TrimSpace(audit.EvidenceText) != "" {
		logInput = audit.EvidenceText
	}
	log := s.buildLog(input, cfg, action, flagged, category, score, scores, logInput, nil, nil, "")
	log.MatchedKeyword = keyword
	applyUnifiedModerationAudit(log, fragment, cfg, audit)
	if !flagged {
		log.ViolationCount = 0
		log.DispositionStatus = "not_counted"
	}
	applySideEffects := flagged && !audit.CacheHit && action != ContentModerationActionCacheBlock
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
		Flagged:         flagged,
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
	log.DeepSeekConfidence = audit.DeepSeekConfidence
	log.DeepSeekCategory = audit.DeepSeekCategory
	log.DeepSeekReason = audit.DeepSeekReason
	log.ReviewOutcome = audit.ReviewOutcome
	log.ReviewerDisagreement = audit.ReviewerDisagreement
	log.ReviewAttempts = append([]ContentModerationReviewAttempt(nil), audit.ReviewAttempts...)
	if len(audit.ReviewAttempts) > 0 {
		latencyMS := 0
		for _, attempt := range audit.ReviewAttempts {
			if attempt.LatencyMS > 0 {
				latencyMS += attempt.LatencyMS
			}
		}
		log.UpstreamLatencyMS = &latencyMS
	}
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
		len(entry.ModelProfile) + len(entry.PromptVersion) + len(entry.EvidenceMode) + len(entry.ParserStatus) +
		len(entry.Reason) + len(entry.Label) + len(entry.EndpointID) + len(entry.ReviewOutcome) + len(entry.ConsensusStatus) +
		len(entry.ReviewAttempts)*128 + 128
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
	return cfg.enabledYuFengSecondLayerEndpoints()
}

func (cfg *ContentModerationConfig) enabledYuFengSecondLayerEndpoints() []ContentModerationEndpoint {
	if cfg == nil || !cfg.SecondLayerEnabled || !cfg.YuFengEnabled {
		return nil
	}
	all := normalizeContentModerationEndpoints(cfg.SecondLayerEndpoints)
	out := make([]ContentModerationEndpoint, 0, len(all))
	for _, endpoint := range all {
		if endpoint.Enabled && endpoint.Profile == ContentModerationModelProfileYuFengXGuard {
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
			profile != ContentModerationModelProfileYuFengXGuard && profile != "yufeng" && profile != "yufeng-xguard" && profile != "xguard" {
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
