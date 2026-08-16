package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
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
	contentModerationShadowQueueCapacity = 64
)

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
	cache, _ := s.hashCache.(ContentModerationFragmentCache)
	namespace := runtime.fragmentCacheNamespace
	if namespace == "" {
		namespace = cfg.fragmentCacheNamespace()
	}
	if whitelistShadow {
		namespace += contentModerationWhitelistShadowCacheSuffix
	}
	candidates := make([]contentModerationCandidateFragment, 0, len(fragments))
	for _, fragment := range fragments {
		shadowRiskObserved := false
		releaseDecisionLock := s.acquireContentModerationFragmentDecisionLock(namespace + "\x00" + fragment.Hash)
		if cache != nil {
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

		if cfg.KeywordBlockingMode != ContentModerationKeywordModeAPIOnly && runtime.keywordMatcher != nil {
			if keyword, hit := runtime.keywordMatcher.Match(fragment.Text); hit {
				if suppressToolDocumentationKeyword(fragment, keyword) {
					keyword, hit = runtime.keywordMatcher.Match(withoutPowerShellDocumentationCommands(fragment.Text))
				}
				if hit {
					hardMatches := contentModerationHardMatchesForKeyword(fragment.Text, keyword)
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
						if persisted {
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
		}

		if cfg.SecondLayerEnabled && cfg.KeywordBlockingMode != ContentModerationKeywordModeKeywordOnly {
			candidateSystemReady := runtime.secondLayerPrefilterMatcher != nil
			if !candidateSystemReady {
				slog.Warn("content_moderation.candidate_system_unavailable", "request_id", input.RequestID)
				releaseDecisionLock()
				continue
			}
			candidateMatches := runtime.secondLayerPrefilterMatcher.MatchAllExcluding(fragment.Text, cfg.KeywordAllowlist)
			if len(candidateMatches) > 0 {
				candidates = append(candidates, contentModerationCandidateFragment{Fragment: fragment, Matches: candidateMatches, Tier: "candidate"})
				releaseDecisionLock()
				continue
			}
			if !whitelistShadow && !shadowRiskObserved {
				s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
			}
			releaseDecisionLock()
			continue
		}

		if !shadowRiskObserved {
			s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
		}
		releaseDecisionLock()
	}
	if len(candidates) > 0 {
		return s.checkUnifiedCandidateEvidence(ctx, input, cfg, namespace, cache, candidates, whitelistShadow)
	}
	return allow
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
	endpoints := cfg.enabledSecondLayerEndpoints()
	if len(endpoints) == 0 {
		return allow
	}
	limit := endpoints[0].InputLimit
	for _, endpoint := range endpoints[1:] {
		if endpoint.InputLimit < limit {
			limit = endpoint.InputLimit
		}
	}
	bundle := buildContentModerationCandidateEvidence(candidates, limit, cfg)
	if strings.TrimSpace(bundle.Evidence.Text) == "" || bundle.CacheHash == "" {
		slog.Warn("content_moderation.candidate_evidence_empty", "request_id", input.RequestID)
		return allow
	}
	primary := candidates[0].Fragment
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
			_ = s.checkUnifiedCandidateEvidenceBundle(shadowCtx, asyncInput, asyncCfg, namespace, cache, bundle, primary, whitelistShadow)
		})
		if !enqueued {
			slog.Warn("content_moderation.second_layer_shadow_queue_full", "request_id", input.RequestID)
		}
		return allow
	}
	return s.checkUnifiedCandidateEvidenceBundle(ctx, input, cfg, namespace, cache, bundle, primary, whitelistShadow)
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
) *ContentModerationDecision {
	allow := &ContentModerationDecision{Allowed: true, Action: ContentModerationActionAllow}
	cacheFragment := bundle.Fragment
	cacheFragment.Hash = bundle.CacheHash
	cacheFragment.Path = "moderation.evidence_windows"
	releaseDecisionLock := s.acquireContentModerationFragmentDecisionLock(namespace + "\x00" + bundle.CacheHash)
	defer releaseDecisionLock()

	if cache != nil {
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

	result, attempted, err := s.scanUnifiedSecondLayerPrepared(ctx, cfg, contentModerationSecondLayerInput{
		Fragment: bundle.Fragment, Evidence: bundle.Evidence, KeywordTier: "candidate", KeywordRuleID: bundle.PrimaryRuleID,
		Background: whitelistShadow || cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow,
	})
	if err != nil {
		if !errors.Is(err, errContentModerationSecondLayerBusy) {
			slog.Warn("content_moderation.second_layer_failed", "error", err)
		}
		return allow
	}
	if !attempted {
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
		if persisted {
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
	log := s.buildLog(input, cfg, action, false, category, highestScore, scores, logInput, nil, nil, "")
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
