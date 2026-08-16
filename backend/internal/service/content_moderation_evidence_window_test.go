package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContentModerationPrefilterMatchAllMapsNormalizedUnicodeOffsets(t *testing.T) {
	text := "前缀🙂 REVERSE---SHELL，然后是降级---攻击。"
	matcher := newContentModerationPrefilterMatcher([]string{"reverse shell", "降级 攻击"})
	matches := matcher.MatchAll(text)
	require.Len(t, matches, 2)
	runes := []rune(text)
	require.Equal(t, "REVERSE---SHELL", string(runes[matches[0].Start:matches[0].End]))
	require.Equal(t, "降级---攻击", string(runes[matches[1].Start:matches[1].End]))
	require.Equal(t, "reverse shell", matches[0].Keyword)
	require.Equal(t, "降级 攻击", matches[1].Keyword)
}

func TestContentModerationKeywordMatchAllBoundsRepeatedLargeInput(t *testing.T) {
	text := strings.Repeat("reverse---shell diagnostic ", 1<<17)
	prefilterMatches := newContentModerationPrefilterMatcher([]string{"reverse shell"}).MatchAll(text)
	require.Len(t, prefilterMatches, contentModerationKeywordMatchLimit)
	require.Equal(t, "reverse shell", prefilterMatches[0].Keyword)

	hardText := strings.Repeat("blocked ", 1<<18)
	hardMatches := newContentModerationKeywordMatcher([]string{"blocked"}).MatchAll(hardText)
	require.Len(t, hardMatches, contentModerationKeywordMatchLimit)
	require.Equal(t, "blocked", hardMatches[0].Keyword)
}

func TestContentModerationHardEvidenceFindsSelectedKeywordBeyondGlobalMatchLimit(t *testing.T) {
	text := strings.Repeat("noise ", contentModerationKeywordMatchLimit+10) + "selected"
	matcher := newContentModerationKeywordMatcher([]string{"selected", "noise"})
	keyword, matched := matcher.Match(text)
	require.True(t, matched)
	require.Equal(t, "selected", keyword)

	matches := contentModerationHardMatchesForKeyword(text, keyword)
	require.Len(t, matches, 1)
	require.Equal(t, "selected", matches[0].Keyword)
}

func TestContentModerationCandidateEvidenceKeepsLocalNegationAndMiddleLogLines(t *testing.T) {
	text := strings.Join([]string{
		"2026-08-16 service starting",
		"ordinary line before",
		"do not execute reverse---shell; this is quoted diagnostic text",
		"ordinary line after",
		strings.Repeat("irrelevant tail ", 200),
	}, "\n")
	fragment, ok := newContentModerationFragment("tool", "text", "messages.2.content", text)
	require.True(t, ok)
	matcher := newContentModerationPrefilterMatcher([]string{"reverse shell"})
	bundle := buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
		Fragment: fragment, Matches: matcher.MatchAll(text), Tier: "candidate",
	}}, 4096, defaultContentModerationConfig())

	require.Len(t, bundle.Windows, 1)
	require.Contains(t, bundle.Evidence.Text, "do not execute")
	require.Contains(t, bundle.Evidence.Text, "ordinary line before")
	require.Contains(t, bundle.Evidence.Text, "ordinary line after")
	require.NotContains(t, bundle.Evidence.Text, "irrelevant tail irrelevant tail irrelevant tail")
	require.LessOrEqual(t, len([]rune(bundle.Evidence.Text)), contentModerationEvidenceWindowBudgetRunes)
	require.Len(t, bundle.Windows[0].Matches, 1)
	match := bundle.Windows[0].Matches[0]
	require.Equal(t, "candidate", match.Tier)
	windowRunes := []rune(bundle.Windows[0].Text)
	require.Equal(t, "reverse---shell", strings.ToLower(string(windowRunes[match.Start:match.End])))
}

func TestContentModerationCandidateEvidenceKeepsShortContextUnchanged(t *testing.T) {
	text := "BEGIN " + strings.Repeat("ordinary ", 12) + "reverse shell " + strings.Repeat("routine ", 12) + "END"
	bundle := buildCandidateEvidenceForTest(t, ContentModerationContextTool, text, []string{"reverse shell"})

	require.Len(t, bundle.Windows, 1)
	require.Equal(t, text, bundle.Windows[0].Text)
	require.LessOrEqual(t, len([]rune(bundle.Windows[0].Text)), contentModerationEvidenceDefaultMatchRunes)
	require.False(t, bundle.Evidence.Truncated)
	require.False(t, bundle.Evidence.Segments[0].Truncated)
}

func TestContentModerationCandidateEvidenceAdaptivelyExpandsContextTo480Runes(t *testing.T) {
	text := "BEGIN " + strings.Repeat("ordinary ", 20) + "reverse shell " + strings.Repeat("routine ", 20) + "END"
	bundle := buildCandidateEvidenceForTest(t, ContentModerationContextUser, text, []string{"reverse shell"})

	require.Len(t, bundle.Windows, 1)
	require.Equal(t, text, bundle.Windows[0].Text)
	require.Greater(t, len([]rune(bundle.Windows[0].Text)), contentModerationEvidenceDefaultMatchRunes)
	require.LessOrEqual(t, len([]rune(bundle.Windows[0].Text)), contentModerationEvidenceExpandedMatchRunes)
	require.Contains(t, bundle.Windows[0].Text, "BEGIN")
	require.Contains(t, bundle.Windows[0].Text, "END")
	require.False(t, bundle.Evidence.Truncated)
	require.False(t, bundle.Evidence.Segments[0].Truncated)
}

func TestContentModerationCandidateEvidenceCapsExpandedContextAt480Runes(t *testing.T) {
	text := "BEGIN " + strings.Repeat("ordinary ", 35) + "reverse shell " + strings.Repeat("routine ", 35) + "END"
	bundle := buildCandidateEvidenceForTest(t, ContentModerationContextTool, text, []string{"reverse shell"})

	require.Len(t, bundle.Windows, 1)
	require.Len(t, []rune(bundle.Windows[0].Text), contentModerationEvidenceExpandedMatchRunes)
	require.Contains(t, bundle.Windows[0].Text, "reverse shell")
	require.True(t, bundle.Evidence.Truncated)
	require.True(t, bundle.Evidence.Segments[0].Truncated)
}

func TestContentModerationCandidateEvidenceMergedWindowsStayWithin480Runes(t *testing.T) {
	text := strings.Repeat("alpha ", 45) + "reverse shell " + strings.Repeat("middle ", 12) + "session hijack " + strings.Repeat("omega ", 45)
	bundle := buildCandidateEvidenceForTest(t, ContentModerationContextTool, text, []string{"reverse shell", "session hijack"})

	require.Len(t, bundle.Windows, 2)
	for _, window := range bundle.Windows {
		require.LessOrEqual(t, len([]rune(window.Text)), contentModerationEvidenceExpandedMatchRunes)
	}
	require.Contains(t, bundle.Evidence.Text, "reverse shell")
	require.Contains(t, bundle.Evidence.Text, "session hijack")
	require.LessOrEqual(t, len([]rune(bundle.Evidence.Text)), contentModerationEvidenceWindowBudgetRunes)
}

func TestContentModerationCandidateEvidenceCapsRedactionExpansionAroundKeyword(t *testing.T) {
	text := strings.Repeat("token=x ", 50) + "reverse shell near the end"
	bundle := buildCandidateEvidenceForTest(t, ContentModerationContextTool, text, []string{"reverse shell"})

	require.Len(t, bundle.Windows, 1)
	window := bundle.Windows[0]
	require.LessOrEqual(t, len([]rune(window.Text)), contentModerationEvidenceExpandedMatchRunes)
	require.Contains(t, window.Text, "reverse shell", "post-redaction cropping must stay centered on the candidate match")
	require.Len(t, window.Matches, 1)
	windowRunes := []rune(window.Text)
	require.Equal(t, "reverse shell", string(windowRunes[window.Matches[0].Start:window.Matches[0].End]))
	require.True(t, bundle.Evidence.Truncated)
	require.True(t, bundle.Evidence.Segments[0].Truncated)
}

func buildCandidateEvidenceForTest(t *testing.T, contextClass, text string, keywords []string) contentModerationEvidenceBundle {
	t.Helper()
	role := "tool"
	if contextClass == ContentModerationContextUser || contextClass == ContentModerationContextAssistant {
		role = contextClass
	}
	fragment, ok := newContentModerationFragment(role, "text", "messages.0.content", text)
	require.True(t, ok)
	fragment.ContextClass = contextClass
	matcher := newContentModerationPrefilterMatcher(keywords)
	return buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
		Fragment: fragment, Matches: matcher.MatchAll(text), Tier: "candidate",
	}}, 4096, defaultContentModerationConfig())
}

func TestContentModerationCandidateEvidenceMultipleFragmentsCallsModelOnce(t *testing.T) {
	var calls atomic.Int64
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: none"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	cfg.RecordNonHits = true
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true, config: cfg,
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher([]string{"reverse shell", "session hijack"}),
		fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
	}
	repo := &contentModerationTestRepo{}
	svc := &ContentModerationService{repo: repo}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")
	decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Scope: &scope, Protocol: ContentModerationProtocolOpenAIChat,
		Body: []byte(`{"messages":[{"role":"user","content":"Do not build a reverse shell; explain the phrase."},{"role":"tool","content":"audit found session---hijacking marker in a fixture"}]}`),
	}, runtime)

	require.True(t, decision.Allowed)
	require.Equal(t, int64(1), calls.Load())
	require.Len(t, repo.logs, 1)
	require.Len(t, repo.logs[0].EvidenceWindows, 2)
	require.Equal(t, "reverse shell", repo.logs[0].MatchedKeyword)
	require.NotEmpty(t, body)
}

func TestContentModerationCandidateEvidenceLongRepeatedMatchesIsBoundedAndSingleCall(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: none"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	cfg.RecordNonHits = true
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true, config: cfg,
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher([]string{"reverse shell"}),
	}
	svc := &ContentModerationService{repo: &contentModerationTestRepo{}}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")
	longText := strings.Repeat("line reverse shell diagnostic\n", 1000)
	body, err := json.Marshal(map[string]any{"input": longText})
	require.NoError(t, err)
	decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Scope: &scope, Protocol: ContentModerationProtocolOpenAIResponses, Body: body,
	}, runtime)
	require.True(t, decision.Allowed)
	require.Equal(t, int64(1), calls.Load(), "truncation must not trigger first/last fallback calls")
}

func TestContentModerationCandidateAllowlistOnlySuppressesOverlappingMatch(t *testing.T) {
	text := "quoted reverse shell example; independent session hijack request"
	filtered := newContentModerationPrefilterMatcher([]string{"reverse shell", "session hijack"}).MatchAllExcluding(
		text, []string{"quoted reverse shell example"},
	)
	require.Len(t, filtered, 1)
	require.Equal(t, "session hijack", filtered[0].Keyword)
}

func TestContentModerationCandidateAllowlistDoesNotConsumeMatchBudget(t *testing.T) {
	allowed := strings.Repeat("quoted reverse shell example; ", contentModerationKeywordMatchLimit+10)
	text := allowed + "independent session hijack request"
	matches := newContentModerationPrefilterMatcher([]string{"reverse shell", "session hijack"}).MatchAllExcluding(
		text, []string{"quoted reverse shell example"},
	)
	require.Len(t, matches, 1)
	require.Equal(t, "session hijack", matches[0].Keyword)
	runes := []rune(text)
	require.Equal(t, "session hijack", string(runes[matches[0].Start:matches[0].End]))
}

func TestContentModerationCandidateAllowlistSuppressesEarlierEndingOverlap(t *testing.T) {
	matches := newContentModerationPrefilterMatcher([]string{"shell example"}).MatchAllExcluding(
		"reverse shell example", []string{"reverse shell"},
	)

	require.Empty(t, matches)
}

func TestContentModerationCandidateAllowlistBudgetStillCallsModelForTailRisk(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: none"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	cfg.RecordNonHits = true
	cfg.KeywordAllowlist = []string{"quoted reverse shell example"}
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true, config: cfg,
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher([]string{"reverse shell", "session hijack"}),
		fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
	}
	repo := &contentModerationTestRepo{}
	svc := &ContentModerationService{repo: repo}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-allowlist-budget")
	text := strings.Repeat("quoted reverse shell example; ", contentModerationKeywordMatchLimit+10) + "independent session hijack request"
	body, err := json.Marshal(map[string]any{"input": text})
	require.NoError(t, err)
	decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Scope: &scope, Protocol: ContentModerationProtocolOpenAIResponses, Body: body,
	}, runtime)

	require.True(t, decision.Allowed)
	require.Equal(t, int64(1), calls.Load())
	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Len(t, logs[0].EvidenceWindows, 1)
	require.Equal(t, "session hijack", logs[0].EvidenceWindows[0].Matches[0].Keyword)
}

func TestContentModerationEvidenceCacheHashTracksPolicyRulesContextAndModel(t *testing.T) {
	fragment, ok := newContentModerationFragment("user", "text", "input", "explain reverse shell safely")
	require.True(t, ok)
	matcher := newContentModerationPrefilterMatcher([]string{"reverse shell"})
	cfg := secondLayerGateTestConfig("http://127.0.0.1:1")
	bundle := buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
		Fragment: fragment, Matches: matcher.MatchAll(fragment.Text), Tier: "candidate",
	}}, 4096, cfg)
	require.NotEmpty(t, bundle.CacheHash)

	changedModel := cloneContentModerationConfig(cfg)
	changedModel.SecondLayerEndpoints[0].Model = "guard-v2"
	modelBundle := buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
		Fragment: fragment, Matches: matcher.MatchAll(fragment.Text), Tier: "candidate",
	}}, 4096, changedModel)
	require.NotEqual(t, bundle.CacheHash, modelBundle.CacheHash)

	toolFragment := fragment
	toolFragment.ContextClass = ContentModerationContextTool
	contextBundle := buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
		Fragment: toolFragment, Matches: matcher.MatchAll(toolFragment.Text), Tier: "candidate",
	}}, 4096, cfg)
	require.NotEqual(t, bundle.CacheHash, contextBundle.CacheHash)
	require.Equal(t, ContentModerationEvidencePolicyVersion, defaultContentModerationConfig().EvidencePolicyVersion)
}

func TestContentModerationEvidencePolicyMigrationNormalizesKnownVersions(t *testing.T) {
	for _, version := range []string{"", contentModerationLegacyEvidencePolicyVersion, contentModerationPreviousEvidencePolicyVersion} {
		cfg := defaultContentModerationConfig()
		cfg.EvidencePolicyVersion = version
		cfg.normalize()
		require.Equal(t, ContentModerationEvidencePolicyVersion, cfg.EvidencePolicyVersion)
	}

	cfg := defaultContentModerationConfig()
	cfg.EvidencePolicyVersion = "site-keyword-windows-v9"
	cfg.normalize()
	require.Equal(t, "site-keyword-windows-v9", cfg.EvidencePolicyVersion)
}

func TestContentModerationCandidateEvidenceKeepsDistantMatchesOnOneLongLine(t *testing.T) {
	text := "reverse shell " + strings.Repeat("neutral ", 300) + "session hijack"
	fragment, ok := newContentModerationFragment("tool", "text", "messages.0.content", text)
	require.True(t, ok)
	matcher := newContentModerationPrefilterMatcher([]string{"reverse shell", "session hijack"})
	bundle := buildContentModerationCandidateEvidence([]contentModerationCandidateFragment{{
		Fragment: fragment, Matches: matcher.MatchAll(text), Tier: "candidate",
	}}, 4096, defaultContentModerationConfig())
	require.Len(t, bundle.Windows, 2)
	require.Contains(t, bundle.Evidence.Text, "reverse shell")
	require.Contains(t, bundle.Evidence.Text, "session hijack")
	require.LessOrEqual(t, len([]rune(bundle.Evidence.Text)), contentModerationEvidenceWindowBudgetRunes)
}

func TestContentModerationCandidateHealthRejectsEmptyPolicyAndKeepsLastKnownGood(t *testing.T) {
	invalid := defaultContentModerationConfig()
	invalid.SecondLayerEnabled = true
	invalid.KeywordBlockingMode = ContentModerationKeywordModeAPIOnly
	invalid.CandidateEnabled = false
	invalid.BlockedKeywords = nil
	invalid.HardBlockPatterns = nil
	invalid.CandidateKeywords = nil
	_, err := effectiveContentModerationSecondLayerKeywords(invalid)
	require.ErrorContains(t, err, "candidate keywords are empty")

	valid := cloneContentModerationConfig(invalid)
	valid.CandidateKeywords = []string{"reverse shell"}
	matcher := newContentModerationPrefilterMatcher(valid.CandidateKeywords)
	svc := &ContentModerationService{}
	current := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true, config: valid, secondLayerPrefilterMatcher: matcher,
	}
	svc.runtimeSnapshot.Store(current)
	svc.replaceRuntimeConfig(invalid, []byte(`{"invalid":"candidate-policy"}`))
	require.Same(t, current, svc.runtimeSnapshot.Load())
	require.Same(t, matcher, svc.runtimeSnapshot.Load().secondLayerPrefilterMatcher)
}

func TestContentModerationHardBlockCacheReplayRebuildsEvidenceWindows(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordOnly
	cfg.BlockedKeywords = []string{"dangerous operation"}
	cfg.normalize()
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := &ContentModerationService{repo: repo, hashCache: cache}
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true, config: cfg,
		keywordMatcher:         newContentModerationKeywordMatcher(cfg.BlockedKeywords),
		fragmentCacheNamespace: cfg.fragmentCacheNamespace(),
	}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")
	input := ContentModerationCheckInput{
		Scope: &scope, Protocol: ContentModerationProtocolOpenAIResponses,
		Body: []byte(`{"input":"do not perform dangerous operation in this example"}`),
	}
	first := svc.checkUnifiedFragments(context.Background(), input, runtime)
	second := svc.checkUnifiedFragments(context.Background(), input, runtime)
	require.True(t, first.Blocked)
	require.True(t, second.Blocked)
	require.Equal(t, ContentModerationActionCacheBlock, second.Action)
	logs := repo.snapshotLogs()
	require.Len(t, logs, 2)
	require.NotEmpty(t, logs[1].EvidenceWindows)
	require.Equal(t, "dangerous operation", logs[1].EvidenceWindows[0].Matches[0].Keyword)
	require.Equal(t, "keyword_windows", logs[0].EvidenceMode)
	require.Equal(t, "keyword_windows", logs[1].EvidenceMode)
}

func TestContentModerationShadowReviewIsAsynchronousAndBounded(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: none"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	cfg.SecondLayerStage = ContentModerationSecondLayerStageShadow
	repo := &contentModerationTestRepo{}
	svc := &ContentModerationService{repo: repo}
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true, config: cfg,
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher([]string{"reverse shell"}),
	}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")
	begin := time.Now()
	decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Scope: &scope, Protocol: ContentModerationProtocolOpenAIResponses,
		Body: []byte(`{"input":"quote reverse shell for analysis"}`),
	}, runtime)
	require.True(t, decision.Allowed)
	require.Less(t, time.Since(begin), 250*time.Millisecond)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shadow worker did not start")
	}
	require.Equal(t, int64(1), svc.secondLayerShadowQueued.Load())
	close(release)
	require.Eventually(t, func() bool {
		return svc.secondLayerShadowDone.Load() == 1 && len(repo.snapshotLogs()) == 1
	}, time.Second, 10*time.Millisecond)

	block := make(chan struct{})
	blockStarted := make(chan struct{})
	require.True(t, svc.enqueueContentModerationShadowReview(func() { close(blockStarted); <-block }))
	select {
	case <-blockStarted:
	case <-time.After(time.Second):
		t.Fatal("bounded queue worker did not start blocking job")
	}
	for range contentModerationShadowQueueCapacity {
		require.True(t, svc.enqueueContentModerationShadowReview(func() {}))
	}
	require.False(t, svc.enqueueContentModerationShadowReview(func() {}))
	require.Equal(t, int64(1), svc.secondLayerShadowDropped.Load())
	close(block)
	require.Eventually(t, func() bool { return svc.secondLayerShadowDone.Load() == 66 }, time.Second, 10*time.Millisecond)
}
