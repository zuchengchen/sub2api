package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	for _, fragment := range fragments {
		whitelistRiskObserved := false
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
					if whitelistShadow {
						whitelistRiskObserved = true
						s.persistUnifiedShadowAudit(ctx, input, cfg, fragment, ContentModerationActionWhitelistShadow, category, keyword, unifiedModerationAudit{
							CacheHit: true, DecisionSource: "cache_replay_whitelist_shadow", SourceLogID: entry.SourceLogID,
							ReplayOfInputHash: defaultContentModerationString(entry.ReplayOfInputHash, fragment.Hash), CacheNamespace: namespace,
							ModelProfile: entry.ModelProfile, PromptVersion: entry.PromptVersion, EvidenceMode: entry.EvidenceMode,
							EvidenceTruncated: entry.EvidenceTruncated, ParserStatus: entry.ParserStatus,
							KeywordTier: entry.KeywordTier, KeywordRuleID: entry.KeywordRuleID,
						})
						releaseDecisionLock()
						continue
					}
					decision, _, _ := s.unifiedBlockDecisionWithAudit(ctx, input, cfg, fragment, ContentModerationActionCacheBlock, category, keyword, unifiedModerationAudit{
						CacheHit: true, DecisionSource: "cache_replay", SourceLogID: entry.SourceLogID,
						ReplayOfInputHash: defaultContentModerationString(entry.ReplayOfInputHash, fragment.Hash), CacheNamespace: namespace,
						ModelProfile: entry.ModelProfile, PromptVersion: entry.PromptVersion, EvidenceMode: entry.EvidenceMode,
						EvidenceTruncated: entry.EvidenceTruncated, ParserStatus: entry.ParserStatus,
						KeywordTier: entry.KeywordTier, KeywordRuleID: entry.KeywordRuleID,
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
					audit := unifiedModerationAudit{
						DecisionSource: "keyword_high_confidence", CacheNamespace: namespace, KeywordTier: "high_confidence", KeywordRuleID: contentModerationKeywordRuleID(keyword),
					}
					if whitelistShadow {
						whitelistRiskObserved = true
						audit.DecisionSource = "keyword_high_confidence_whitelist_shadow"
						s.persistUnifiedShadowAudit(ctx, input, cfg, fragment, ContentModerationActionWhitelistShadow, contentModerationKeywordCategory, keyword, audit)
					} else {
						decision, log, persisted := s.unifiedBlockDecisionWithAudit(ctx, input, cfg, fragment, ContentModerationActionKeywordBlock, contentModerationKeywordCategory, keyword, audit)
						if persisted {
							s.putUnifiedFragmentCacheEntry(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentCacheEntry{
								Result: ContentModerationFragmentBlock, SourceLogID: contentModerationLogIDPtr(log), ReplayOfInputHash: fragment.Hash,
								DecisionSource: "keyword_high_confidence", Category: contentModerationKeywordCategory, MatchedKeyword: keyword,
								KeywordTier: "high_confidence", KeywordRuleID: contentModerationKeywordRuleID(keyword),
							})
						}
						releaseDecisionLock()
						return decision
					}
				}
			}
		}

		if cfg.SecondLayerEnabled && cfg.KeywordBlockingMode != ContentModerationKeywordModeKeywordOnly {
			keywordTier := "candidate_unavailable"
			keywordRuleID := ""
			candidateKeyword := ""
			candidateHit := false
			candidateSystemReady := runtime.secondLayerPrefilterMatcher != nil
			if candidateSystemReady {
				candidateKeyword, candidateHit = runtime.secondLayerPrefilterMatcher.Match(fragment.Text)
			}
			if candidateHit && matchesContentModerationAllowlist(fragment.Text, cfg.KeywordAllowlist) {
				candidateHit = false
			}
			switch {
			case candidateHit:
				keywordTier = "candidate"
				keywordRuleID = contentModerationKeywordRuleID(candidateKeyword)
			case candidateSystemReady:
				if whitelistShadow {
					keywordTier = "whitelist_shadow"
				} else {
					s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
					releaseDecisionLock()
					continue
				}
			}
			result, attempted, err := s.scanUnifiedSecondLayerFragmentWithTier(ctx, cfg, fragment, keywordTier, keywordRuleID)
			if err != nil {
				if !errors.Is(err, errContentModerationSecondLayerBusy) {
					slog.Warn("content_moderation.second_layer_failed", "error", err)
				}
				releaseDecisionLock()
				continue
			}
			if attempted && result.Blocked {
				if whitelistShadow || cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow {
					action := ContentModerationActionSecondLayerShadow
					decisionSource := "model_shadow"
					if whitelistShadow {
						whitelistRiskObserved = true
						action = ContentModerationActionWhitelistShadow
						decisionSource = "model_whitelist_shadow"
					}
					s.persistUnifiedShadowAudit(ctx, input, cfg, fragment, action, result.Category, "", unifiedModerationAudit{
						DecisionSource: decisionSource, CacheNamespace: namespace, ModelProfile: result.Profile,
						PromptVersion: result.PromptVersion, EvidenceMode: result.EvidenceMode,
						EvidenceTruncated: result.EvidenceTruncated, ParserStatus: result.ParserStatus,
						KeywordTier: result.KeywordTier, KeywordRuleID: result.KeywordRuleID,
					})
					if !whitelistRiskObserved {
						s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
					}
					releaseDecisionLock()
					continue
				}
				decision, log, persisted := s.unifiedBlockDecisionWithAudit(ctx, input, cfg, fragment, ContentModerationActionSecondLayerBlock, result.Category, "", unifiedModerationAudit{
					DecisionSource: "model", CacheNamespace: namespace, ModelProfile: result.Profile,
					PromptVersion: result.PromptVersion, EvidenceMode: result.EvidenceMode,
					EvidenceTruncated: result.EvidenceTruncated, ParserStatus: result.ParserStatus,
					KeywordTier: result.KeywordTier, KeywordRuleID: result.KeywordRuleID,
				})
				if persisted {
					s.putUnifiedFragmentCacheEntry(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentCacheEntry{
						Result: ContentModerationFragmentBlock, SourceLogID: contentModerationLogIDPtr(log), ReplayOfInputHash: fragment.Hash,
						DecisionSource: "model", Category: result.Category, ModelProfile: result.Profile,
						PromptVersion: result.PromptVersion, KeywordTier: result.KeywordTier, KeywordRuleID: result.KeywordRuleID, EvidenceMode: result.EvidenceMode,
						EvidenceTruncated: result.EvidenceTruncated, ParserStatus: result.ParserStatus,
					})
				}
				releaseDecisionLock()
				return decision
			}
			if attempted {
				if cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow || cfg.RecordNonHits {
					action := ContentModerationActionAllow
					decisionSource := "model"
					if cfg.SecondLayerStage == ContentModerationSecondLayerStageShadow {
						action = ContentModerationActionSecondLayerShadow
						decisionSource = "model_shadow"
					}
					log := s.buildLog(input, cfg, action, false, "", 0, nil, fragment.Text, nil, nil, "")
					applyUnifiedModerationAudit(log, fragment, cfg, unifiedModerationAudit{
						DecisionSource: decisionSource, CacheNamespace: namespace, ModelProfile: result.Profile,
						PromptVersion: result.PromptVersion, EvidenceMode: result.EvidenceMode,
						EvidenceTruncated: result.EvidenceTruncated, ParserStatus: result.ParserStatus,
						KeywordTier: result.KeywordTier, KeywordRuleID: result.KeywordRuleID,
					})
					s.persistContentModerationLogWithInput(ctx, cfg, log, fragment.Hash, false, false, &input)
				}
				if !whitelistRiskObserved {
					s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
				}
			}
			releaseDecisionLock()
			continue
		}

		if !whitelistRiskObserved {
			s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
		}
		releaseDecisionLock()
	}
	return allow
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

func matchesContentModerationAllowlist(text string, values []string) bool {
	text = strings.ToLower(text)
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" && strings.Contains(text, value) {
			return true
		}
	}
	return false
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
	log := s.buildLog(input, cfg, action, false, category, highestScore, scores, fragment.Text, nil, nil, "")
	log.MatchedKeyword = keyword
	applyUnifiedModerationAudit(log, fragment, cfg, audit)
	s.persistContentModerationLogWithInput(ctx, cfg, log, fragment.Hash, false, false, &input)
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
}

func (s *ContentModerationService) unifiedBlockDecisionWithAudit(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, fragment ContentModerationFragment, action, category, keyword string, audit unifiedModerationAudit) (*ContentModerationDecision, *ContentModerationLog, bool) {
	if category == "" {
		category = "content_policy"
	}
	scores := map[string]float64{category: 1}
	log := s.buildLog(input, cfg, action, true, category, 1, scores, fragment.Text, nil, nil, "")
	log.MatchedKeyword = keyword
	applyUnifiedModerationAudit(log, fragment, cfg, audit)
	applySideEffects := !audit.CacheHit && action != ContentModerationActionCacheBlock
	persisted := s.persistContentModerationLogWithInput(ctx, cfg, log, fragment.Hash, false, applySideEffects, &input)
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
		InputHash:       fragment.Hash,
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
	if audit.CacheHit {
		log.ViolationCount = 0
		log.DispositionStatus = "not_counted"
	}
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
	if stage := strings.TrimSpace(cfg.SecondLayerStage); stage != "" && stage != ContentModerationSecondLayerStageEnforce && stage != ContentModerationSecondLayerStageShadow {
		return fmt.Errorf("unsupported second-layer stage %q", cfg.SecondLayerStage)
	}
	if strings.TrimSpace(cfg.CacheVersion) == "" {
		return fmt.Errorf("content moderation cache version is required")
	}
	return nil
}
