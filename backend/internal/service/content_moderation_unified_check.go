package service

import (
	"context"
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

	fragments := ExtractContentModerationFragments(input.Protocol, input.Body)
	if len(fragments) == 0 {
		return allow
	}
	cache, _ := s.hashCache.(ContentModerationFragmentCache)
	namespace := runtime.fragmentCacheNamespace
	if namespace == "" {
		namespace = cfg.fragmentCacheNamespace()
	}
	for _, fragment := range fragments {
		if cache != nil {
			result, found, err := cache.GetFragmentResult(ctx, namespace, fragment.Hash)
			if err != nil {
				s.fragmentCacheErrors.Add(1)
				slog.Warn("content_moderation.fragment_cache_get_failed", "error", err)
			} else if !found {
				s.fragmentCacheMisses.Add(1)
			} else {
				s.fragmentCacheHits.Add(1)
				switch result {
				case ContentModerationFragmentAllow:
					continue
				case ContentModerationFragmentBlock:
					category := "fragment_cache"
					keyword := ""
					if cfg.KeywordBlockingMode != ContentModerationKeywordModeAPIOnly && runtime.keywordMatcher != nil {
						if matched, hit := runtime.keywordMatcher.Match(fragment.Text); hit {
							category = contentModerationKeywordCategory
							keyword = matched
						}
					}
					return s.unifiedBlockDecision(ctx, input, cfg, fragment, ContentModerationActionCacheBlock, category, keyword)
				}
			}
		}

		if cfg.KeywordBlockingMode != ContentModerationKeywordModeAPIOnly && runtime.keywordMatcher != nil {
			if keyword, hit := runtime.keywordMatcher.Match(fragment.Text); hit {
				s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentBlock)
				return s.unifiedBlockDecision(ctx, input, cfg, fragment, ContentModerationActionKeywordBlock, contentModerationKeywordCategory, keyword)
			}
		}

		if cfg.SecondLayerEnabled && cfg.KeywordBlockingMode != ContentModerationKeywordModeKeywordOnly {
			if cfg.CandidateEnabled {
				if runtime.secondLayerPrefilterMatcher == nil {
					s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
					continue
				}
				if _, hit := runtime.secondLayerPrefilterMatcher.Match(fragment.Text); !hit {
					s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
					continue
				}
			}
			result, attempted, err := s.scanUnifiedSecondLayer(ctx, cfg, fragment.Text)
			if err != nil {
				if !errors.Is(err, errContentModerationSecondLayerBusy) {
					slog.Warn("content_moderation.second_layer_failed", "error", err)
				}
				continue
			}
			if attempted && result.Blocked {
				s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentBlock)
				return s.unifiedBlockDecision(ctx, input, cfg, fragment, ContentModerationActionSecondLayerBlock, result.Category, "")
			}
			if attempted {
				s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
			}
			continue
		}

		s.putUnifiedFragmentCache(ctx, cache, namespace, cfg, fragment, ContentModerationFragmentAllow)
	}
	return allow
}

func (s *ContentModerationService) putUnifiedFragmentCache(ctx context.Context, cache ContentModerationFragmentCache, namespace string, cfg *ContentModerationConfig, fragment ContentModerationFragment, result string) {
	if cache == nil || cfg == nil {
		return
	}
	estimatedBytes := int64(len(fragment.Hash) + len(result) + len(namespace) + 64)
	if err := cache.PutFragmentResult(ctx, namespace, fragment.Hash, result, estimatedBytes, cfg.CacheMaxEntries, cfg.CacheMaxBytes); err != nil {
		s.fragmentCacheWriteErrors.Add(1)
		slog.Warn("content_moderation.fragment_cache_put_failed", "error", err)
	} else {
		s.fragmentCacheWrites.Add(1)
	}
}

func (s *ContentModerationService) unifiedBlockDecision(ctx context.Context, input ContentModerationCheckInput, cfg *ContentModerationConfig, fragment ContentModerationFragment, action, category, keyword string) *ContentModerationDecision {
	if category == "" {
		category = "content_policy"
	}
	scores := map[string]float64{category: 1}
	log := s.buildLog(input, cfg, action, true, category, 1, scores, fragment.Text, nil, nil, "")
	log.MatchedKeyword = keyword
	s.persistContentModerationLogWithInput(ctx, cfg, log, fragment.Hash, false, true, &input)
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
	}
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
	}
	if strings.TrimSpace(cfg.CacheVersion) == "" {
		return fmt.Errorf("content moderation cache version is required")
	}
	return nil
}
