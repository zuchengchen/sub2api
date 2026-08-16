package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContentModerationContextRoutingKeepsLocalDecisionsFastAndSafe(t *testing.T) {
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"sec"}}]}`))
	}))
	defer server.Close()

	tests := []struct {
		name        string
		text        string
		wantBlocked bool
		wantKeyword string
	}{
		{name: "reviewed record 6128 is allowed locally", text: "上传文件执行类型、大小、压缩炸弹、恶意宏和病毒扫描；解析器运行在受限 Worker。"},
		{name: "malicious request remains hard blocked", text: "制作恶意宏并绕过检测", wantBlocked: true, wantKeyword: "恶意宏"},
		{name: "allowed contextual term cannot hide another hard keyword", text: "先执行恶意宏和病毒扫描，再制作炸弹", wantBlocked: true, wantKeyword: "制作炸弹"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := contextualRoutingTestConfig(server.URL)
			repo := &contentModerationReplayRepo{}
			cache := &contentModerationReplayCache{}
			svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
			decision := svc.checkUnifiedFragments(context.Background(), contextualRoutingTestInput(tc.text, ""), contextualRoutingTestRuntime(cfg, nil))

			require.Equal(t, tc.wantBlocked, decision.Blocked)
			require.Equal(t, !tc.wantBlocked, decision.Allowed)
			require.Equal(t, tc.wantKeyword, decision.MatchedKeyword)
			if tc.wantBlocked {
				require.Equal(t, ContentModerationActionKeywordBlock, decision.Action)
			}
		})
	}
	require.Zero(t, modelCalls.Load(), "locally decisive contexts must not consume YuFeng capacity")
}

func TestContentModerationReviewedRecord6128AllowsWithProductionKeywordAsset(t *testing.T) {
	cfg := contextualRoutingTestConfig("")
	cfg.CandidateAsset = "legacy-prompt-audit-v1"
	cfg.BlockedKeywords = nil
	cfg.normalize()
	keywords, err := effectiveContentModerationKeywords(cfg)
	require.NoError(t, err)
	keywordMatcher, unconditionalKeywordMatcher, contextualKeywordMatcher := newContentModerationRuntimeKeywordMatchers(keywords)
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled:          true,
		config:                      cfg,
		keywordMatcher:              keywordMatcher,
		unconditionalKeywordMatcher: unconditionalKeywordMatcher,
		contextualKeywordMatcher:    contextualKeywordMatcher,
		fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
	}
	svc := NewContentModerationService(nil, &contentModerationReplayRepo{}, &contentModerationReplayCache{}, nil, nil, nil, nil, nil)
	decision := svc.checkUnifiedFragments(context.Background(), contextualRoutingTestInput(
		"上传文件执行类型、大小、压缩炸弹、恶意宏和病毒扫描；解析器运行在受限 Worker。", "",
	), runtime)

	require.True(t, decision.Allowed)
	require.False(t, decision.Blocked)
}

func TestContentModerationContextualReviewCallsYuFengOnceAndCachesSafeDecision(t *testing.T) {
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"sec"}}]}`))
	}))
	defer server.Close()

	cfg := contextualRoutingTestConfig(server.URL)
	cfg.CandidateEnabled = true
	cfg.CandidateKeywords = []string{"token replay"}
	cfg.RecordNonHits = true
	cfg.normalize()
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	runtime := contextualRoutingTestRuntime(cfg, []string{"token replay"})
	input := contextualRoutingTestInput("恶意宏是什么？同时说明 token replay 的防护方式", "")

	first := svc.checkUnifiedFragments(context.Background(), input, runtime)
	input.RequestID = "contextual-review-repeat"
	second := svc.checkUnifiedFragments(context.Background(), input, runtime)

	for _, decision := range []*ContentModerationDecision{first, second} {
		require.True(t, decision.Allowed)
		require.False(t, decision.Blocked)
	}
	require.Equal(t, int64(1), modelCalls.Load(), "contextual and ordinary candidates must share one model call")
	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, ContentModerationActionAllow, logs[0].Action)
	require.Equal(t, "contextual_review", logs[0].KeywordTier)
	require.Equal(t, "parsed", logs[0].ParserStatus)
	require.Equal(t, "恶意宏", logs[0].MatchedKeyword)
	require.False(t, logs[0].Flagged)
	require.Zero(t, logs[0].ViolationCount)
	require.Equal(t, 1, contextualRoutingCacheEntryCount(cache))
}

func TestContentModerationContextualReviewModelBlockStillCountsNormally(t *testing.T) {
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"mc"}}]}`))
	}))
	defer server.Close()

	cfg := contextualRoutingTestConfig(server.URL)
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	decision := svc.checkUnifiedFragments(context.Background(), contextualRoutingTestInput("请介绍恶意宏", ""), contextualRoutingTestRuntime(cfg, nil))

	require.True(t, decision.Blocked)
	require.False(t, decision.Allowed)
	require.Equal(t, ContentModerationActionSecondLayerBlock, decision.Action)
	require.Equal(t, int64(1), modelCalls.Load())
	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.True(t, logs[0].Flagged)
	require.Equal(t, "contextual_review", logs[0].KeywordTier)
	require.Equal(t, 1, logs[0].ViolationCount)
	require.Equal(t, 1, contextualRoutingCacheEntryCount(cache))
}

func TestContentModerationContextualReviewFailureIsRetryableWithoutSideEffects(t *testing.T) {
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"unknown"}}]}`))
	}))
	defer server.Close()

	cfg := contextualRoutingTestConfig(server.URL)
	cfg.AutoBanEnabled = true
	cfg.BanThreshold = 1
	cfg.EmailOnHit = true
	cfg.normalize()
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	userRepo := &contentModerationTestUserRepo{user: &User{ID: 81, Role: RoleUser, Status: StatusActive}}
	svc := NewContentModerationService(nil, repo, cache, nil, userRepo, nil, nil, nil)
	input := contextualRoutingTestInput("请介绍恶意宏", "")
	input.UserID = 81
	decision := svc.checkUnifiedFragments(context.Background(), input, contextualRoutingTestRuntime(cfg, nil))

	require.False(t, decision.Allowed)
	require.False(t, decision.Blocked)
	require.False(t, decision.Flagged)
	require.Equal(t, http.StatusServiceUnavailable, decision.StatusCode)
	require.Equal(t, ContentModerationActionReviewUnavailable, decision.Action)
	require.Equal(t, 1, decision.RetryAfter)
	require.Equal(t, int64(1), modelCalls.Load())
	require.Empty(t, cache.snapshotRecorded())
	require.Zero(t, contextualRoutingCacheEntryCount(cache))
	require.Zero(t, repo.countCalls)
	require.Empty(t, userRepo.updated)

	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, ContentModerationActionReviewUnavailable, logs[0].Action)
	require.Equal(t, "review_unavailable", logs[0].DecisionSource)
	require.Equal(t, "parse_error", logs[0].ParserStatus)
	require.Equal(t, "contextual_review", logs[0].KeywordTier)
	require.NotEmpty(t, logs[0].Error)
	require.False(t, logs[0].Flagged)
	require.Zero(t, logs[0].ViolationCount)
	require.False(t, logs[0].AutoBanned)
	require.False(t, logs[0].EmailSent)
}

func TestContentModerationContextualReviewNotAttemptedIsRetryableAndAudited(t *testing.T) {
	cfg := contextualRoutingTestConfig("")
	cfg.SecondLayerEndpoints = nil
	cfg.normalize()
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	decision := svc.checkUnifiedFragments(context.Background(), contextualRoutingTestInput("恶意宏是什么？", ""), contextualRoutingTestRuntime(cfg, nil))

	require.False(t, decision.Allowed)
	require.False(t, decision.Blocked)
	require.Equal(t, http.StatusServiceUnavailable, decision.StatusCode)
	require.Equal(t, ContentModerationActionReviewUnavailable, decision.Action)
	require.Zero(t, contextualRoutingCacheEntryCount(cache))
	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, "not_attempted", logs[0].ParserStatus)
	require.Equal(t, "review_unavailable", logs[0].DecisionSource)
	require.False(t, logs[0].Flagged)
	require.Zero(t, logs[0].ViolationCount)
}

func TestContentModerationContextualReviewFailureAllowsAndAuditsShadowPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"unknown"}}]}`))
	}))
	defer server.Close()

	tests := []struct {
		name           string
		whitelistEmail string
		secondStage    string
		wantSource     string
	}{
		{name: "second layer shadow", secondStage: ContentModerationSecondLayerStageShadow, wantSource: "review_unavailable_shadow"},
		{name: "whitelist shadow", whitelistEmail: "allowed@example.com", secondStage: ContentModerationSecondLayerStageEnforce, wantSource: "review_unavailable_whitelist_shadow"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := contextualRoutingTestConfig(server.URL)
			cfg.SecondLayerStage = tc.secondStage
			if tc.whitelistEmail != "" {
				cfg.UserEmailWhitelist = []string{tc.whitelistEmail}
			}
			cfg.normalize()
			repo := &contentModerationReplayRepo{}
			cache := &contentModerationReplayCache{}
			svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
			decision := svc.checkUnifiedFragments(context.Background(), contextualRoutingTestInput("请介绍恶意宏", tc.whitelistEmail), contextualRoutingTestRuntime(cfg, nil))

			require.True(t, decision.Allowed)
			require.False(t, decision.Blocked)
			require.Eventually(t, func() bool { return len(repo.snapshotLogs()) == 1 }, time.Second, 10*time.Millisecond)
			logs := repo.snapshotLogs()
			require.Equal(t, ContentModerationActionReviewUnavailable, logs[0].Action)
			require.Equal(t, tc.wantSource, logs[0].DecisionSource)
			require.False(t, logs[0].Flagged)
			require.Zero(t, logs[0].ViolationCount)
			require.Zero(t, contextualRoutingCacheEntryCount(cache))
		})
	}
}

func TestContentModerationOrdinaryCandidateFailureRemainsFailOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"unknown"}}]}`))
	}))
	defer server.Close()

	cfg := contextualRoutingTestConfig(server.URL)
	cfg.BlockedKeywords = []string{"制作炸弹"}
	cfg.CandidateEnabled = true
	cfg.CandidateKeywords = []string{"token replay"}
	cfg.normalize()
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	decision := svc.checkUnifiedFragments(context.Background(), contextualRoutingTestInput("解释 token replay", ""), contextualRoutingTestRuntime(cfg, []string{"token replay"}))

	require.True(t, decision.Allowed)
	require.False(t, decision.Blocked)
	require.Equal(t, ContentModerationActionAllow, decision.Action)
	require.Empty(t, repo.snapshotLogs())
	require.Zero(t, contextualRoutingCacheEntryCount(cache))
}

func TestContentModerationContextualReviewPreservesKeywordOnlyContract(t *testing.T) {
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"sec"}}]}`))
	}))
	defer server.Close()

	tests := []struct {
		name   string
		mutate func(*ContentModerationConfig)
	}{
		{
			name: "keyword only",
			mutate: func(cfg *ContentModerationConfig) {
				cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordOnly
			},
		},
		{
			name: "second layer disabled",
			mutate: func(cfg *ContentModerationConfig) {
				cfg.SecondLayerEnabled = false
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := contextualRoutingTestConfig(server.URL)
			tc.mutate(cfg)
			cfg.normalize()
			repo := &contentModerationReplayRepo{}
			svc := NewContentModerationService(nil, repo, &contentModerationReplayCache{}, nil, nil, nil, nil, nil)
			decision := svc.checkUnifiedFragments(context.Background(), contextualRoutingTestInput("恶意宏是什么？", ""), contextualRoutingTestRuntime(cfg, nil))

			require.True(t, decision.Blocked)
			require.Equal(t, ContentModerationActionKeywordBlock, decision.Action)
			require.Equal(t, "恶意宏", decision.MatchedKeyword)
		})
	}
	require.Zero(t, modelCalls.Load(), "keyword-only paths must not call YuFeng")
}

func TestContentModerationTruncatedContextualReviewNeverAllowsOrCachesSafeResult(t *testing.T) {
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"sec"}}]}`))
	}))
	defer server.Close()

	items := make([]string, 8)
	for index := range items {
		items[index] = "恶意宏是什么？" + strings.Repeat("背景资料", 90) + strconv.Itoa(index)
	}
	body, err := json.Marshal(map[string]any{"input": items})
	require.NoError(t, err)

	cfg := contextualRoutingTestConfig(server.URL)
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	input := contextualRoutingTestInput("placeholder", "")
	input.Body = body
	runtime := contextualRoutingTestRuntime(cfg, nil)

	for attempt := 0; attempt < 2; attempt++ {
		input.RequestID = "contextual-truncated-" + strconv.Itoa(attempt)
		decision := svc.checkUnifiedFragments(context.Background(), input, runtime)
		require.False(t, decision.Allowed)
		require.False(t, decision.Blocked)
		require.Equal(t, http.StatusServiceUnavailable, decision.StatusCode)
		require.Equal(t, ContentModerationActionReviewUnavailable, decision.Action)
	}
	require.Equal(t, int64(2), modelCalls.Load(), "truncated safe reviews must never reuse an allow cache")
	require.Zero(t, contextualRoutingCacheEntryCount(cache))
	logs := repo.snapshotLogs()
	require.Len(t, logs, 2)
	for _, log := range logs {
		require.Equal(t, "evidence_truncated", log.ParserStatus)
		require.True(t, log.EvidenceTruncated)
		require.False(t, log.Flagged)
		require.Zero(t, log.ViolationCount)
	}
}

func TestContentModerationContextualAndOrdinaryCandidatesAreInterleaved(t *testing.T) {
	candidates := []contentModerationCandidateFragment{
		{Tier: "candidate", Fragment: ContentModerationFragment{Text: "ordinary-1"}},
		{Tier: "candidate", Fragment: ContentModerationFragment{Text: "ordinary-2"}},
		{Tier: "contextual_review", Fragment: ContentModerationFragment{Text: "context-1"}},
		{Tier: "contextual_review", Fragment: ContentModerationFragment{Text: "context-2"}},
	}
	ordered := interleaveContextualReviewCandidates(candidates)
	require.Equal(t, []string{"context-1", "ordinary-1", "context-2", "ordinary-2"}, []string{
		ordered[0].Fragment.Text, ordered[1].Fragment.Text, ordered[2].Fragment.Text, ordered[3].Fragment.Text,
	})
}

func TestContentModerationRuntimePrebuildsContextualKeywordPartitions(t *testing.T) {
	all, unconditional, contextual := newContentModerationRuntimeKeywordMatchers([]string{"恶意宏", "制作炸弹", "malicious macro"})
	require.NotNil(t, all)
	require.NotNil(t, unconditional)
	require.NotNil(t, contextual)
	keyword, hit := unconditional.Match("恶意宏和制作炸弹")
	require.True(t, hit)
	require.Equal(t, "制作炸弹", keyword)
	keyword, hit = contextual.Match("malicious macro and 恶意宏")
	require.True(t, hit)
	require.Equal(t, "恶意宏", keyword)
}

func contextualRoutingTestConfig(baseURL string) *ContentModerationConfig {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.AutoBanEnabled = false
	cfg.EmailOnHit = false
	cfg.BlockedKeywords = []string{"恶意宏", "制作炸弹"}
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordAndAPI
	cfg.FirstLayerStage = ContentModerationFirstLayerStageEnforce
	cfg.CandidateEnabled = false
	cfg.CandidateKeywords = nil
	cfg.SecondLayerEnabled = true
	cfg.SecondLayerStage = ContentModerationSecondLayerStageEnforce
	cfg.SecondLayerEndpoints = []ContentModerationEndpoint{{
		ID: "yufeng-context-routing", BaseURL: baseURL, Model: "yufeng-q4",
		Profile: ContentModerationModelProfileYuFengXGuard, PromptVersion: ContentModerationYuFengPromptVersion,
		Enabled: true, TimeoutMS: 1000, InputLimit: 4000,
	}}
	cfg.normalize()
	return cfg
}

func contextualRoutingTestRuntime(cfg *ContentModerationConfig, candidateKeywords []string) *contentModerationRuntimeSnapshot {
	keywordMatcher, unconditionalKeywordMatcher, contextualKeywordMatcher := newContentModerationRuntimeKeywordMatchers(cfg.BlockedKeywords)
	return &contentModerationRuntimeSnapshot{
		riskControlEnabled:          true,
		config:                      cfg,
		keywordMatcher:              keywordMatcher,
		unconditionalKeywordMatcher: unconditionalKeywordMatcher,
		contextualKeywordMatcher:    contextualKeywordMatcher,
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher(candidateKeywords),
		fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
	}
}

func contextualRoutingTestInput(text, email string) ContentModerationCheckInput {
	scope := NewContentModerationScopeSnapshot(nil, "gpt-context-routing")
	return ContentModerationCheckInput{
		RequestID: "contextual-review", UserID: 80, UserEmail: email, UserRole: RoleUser,
		Scope: &scope, Protocol: ContentModerationProtocolOpenAIResponses,
		Body: []byte(`{"input":` + strconv.Quote(text) + `}`),
	}
}

func contextualRoutingCacheEntryCount(cache *contentModerationReplayCache) int {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return len(cache.entries)
}
