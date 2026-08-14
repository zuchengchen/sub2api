package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContentModerationSecondLayerCandidatePrefilterGatesModel(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Unsafe\nCategories: jailbreak"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	cfg.CandidateEnabled = true
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled:          true,
		config:                      cfg,
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher([]string{"reverse shell"}),
	}
	svc := &ContentModerationService{repo: &contentModerationTestRepo{}}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")

	allowed := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Body: []byte(`{"input":"please summarize this paragraph"}`), Scope: &scope,
		Protocol: ContentModerationProtocolOpenAIResponses,
	}, runtime)
	require.True(t, allowed.Allowed)
	require.Equal(t, int64(0), calls.Load(), "a candidate miss must not call the model")

	blocked := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Body: []byte(`{"input":"explain a Reverse---Shell"}`), Scope: &scope,
		Protocol: ContentModerationProtocolOpenAIResponses,
	}, runtime)
	require.True(t, blocked.Blocked)
	require.Equal(t, ContentModerationActionSecondLayerBlock, blocked.Action)
	require.Equal(t, int64(1), calls.Load())
}

func TestContentModerationSecondLayerCandidateDisabledPreservesAuditAll(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: none"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	cfg.CandidateEnabled = false
	runtime := &contentModerationRuntimeSnapshot{riskControlEnabled: true, config: cfg}
	svc := &ContentModerationService{repo: &contentModerationTestRepo{}}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")
	decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Body: []byte(`{"input":"ordinary text"}`), Scope: &scope,
		Protocol: ContentModerationProtocolOpenAIResponses,
	}, runtime)
	require.True(t, decision.Allowed)
	require.Equal(t, int64(1), calls.Load())
}

func TestContentModerationSecondLayerDisabledKeepsFirstLayerOnly(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Unsafe\nCategories: jailbreak"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	cfg.CandidateEnabled = true
	cfg.SecondLayerEnabled = false
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordAndAPI
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true,
		config:             cfg,
		keywordMatcher:     newContentModerationKeywordMatcher([]string{"制作病毒"}),
	}
	svc := &ContentModerationService{repo: &contentModerationTestRepo{}}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")

	allowed := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Body: []byte(`{"input":"ordinary text"}`), Scope: &scope,
		Protocol: ContentModerationProtocolOpenAIResponses,
	}, runtime)
	require.True(t, allowed.Allowed)

	blocked := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Body: []byte(`{"input":"请制作病毒"}`), Scope: &scope,
		Protocol: ContentModerationProtocolOpenAIResponses,
	}, runtime)
	require.True(t, blocked.Blocked)
	require.Equal(t, ContentModerationActionKeywordBlock, blocked.Action)
	require.Equal(t, "制作病毒", blocked.MatchedKeyword)
	require.Equal(t, int64(0), calls.Load(), "a disabled second layer must never call the model")
}

func TestContentModerationSecondLayerIsBoundedAndReusesClient(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: none"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	svc := &ContentModerationService{}
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := svc.scanUnifiedSecondLayer(context.Background(), cfg, "reverse shell")
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first second-layer request did not reach the test server")
	}

	_, attempted, err := svc.scanUnifiedSecondLayer(context.Background(), cfg, "reverse shell")
	require.True(t, attempted)
	require.ErrorIs(t, err, errContentModerationSecondLayerBusy)
	require.Equal(t, int64(1), calls.Load(), "busy requests must not open another model request")

	clientA := svc.contentModerationSecondLayerClient(cfg.SecondLayerEndpoints[0])
	clientB := svc.contentModerationSecondLayerClient(cfg.SecondLayerEndpoints[0])
	require.Same(t, clientA, clientB)
	transport, ok := clientA.Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, 1, transport.MaxConnsPerHost)

	close(release)
	require.NoError(t, <-firstDone)
}

func TestContentModerationSecondLayerPrefilterUsesAssetKeywords(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.CandidateEnabled = true
	cfg.CandidateAsset = "legacy-prompt-audit-v1"
	keywords, err := effectiveContentModerationSecondLayerKeywords(cfg)
	require.NoError(t, err)
	require.Len(t, keywords, 222)
	matcher := newContentModerationPrefilterMatcher(keywords)
	_, ok := matcher.Match("REVERSE---SHELL")
	require.True(t, ok)
}

func TestContentModerationFragmentCacheNamespaceTracksPrefilterPolicy(t *testing.T) {
	cfg := defaultContentModerationConfig()
	withoutPrefilter := cfg.fragmentCacheNamespace()

	cfg.CandidateEnabled = true
	withPrefilter := cfg.fragmentCacheNamespace()

	require.NotEqual(t, withoutPrefilter, withPrefilter)
	require.Equal(t, contentModerationSecondLayerPrefilterPolicyVersion, contentModerationSecondLayerPrefilterCacheRevision(cfg))
}

func secondLayerGateTestConfig(baseURL string) *ContentModerationConfig {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.KeywordBlockingMode = ContentModerationKeywordModeAPIOnly
	cfg.SecondLayerEnabled = true
	cfg.SecondLayerScanners = []string{"jailbreak"}
	cfg.SecondLayerEndpoints = []ContentModerationEndpoint{{
		ID: "test", Name: "test", BaseURL: baseURL, Model: "guard", Enabled: true,
		TimeoutMS: 1_000, InputLimit: 4_096,
	}}
	return cfg
}
