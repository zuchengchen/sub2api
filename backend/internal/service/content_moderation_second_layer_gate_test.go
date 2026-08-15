package service

import (
	"context"
	"encoding/json"
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

func TestContentModerationDestructivePayloadDemotionRoutesToSecondLayer(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		modelLabel  string
		wantContext string
		wantBlocked bool
	}{
		{
			name:        "defensive tool documentation is allowed",
			body:        `{"input":[{"type":"function_call_output","call_id":"call_1","output":"OpenClaw-owned writes reject invalid/destructive payloads before commit and save .rejected.*."}]}`,
			modelLabel:  "sec",
			wantContext: ContentModerationContextTool,
		},
		{
			name:        "benign user query is allowed",
			body:        `{"input":"Does this policy mention destructive payload?"}`,
			modelLabel:  "sec",
			wantContext: ContentModerationContextUser,
		},
		{
			name:        "malicious user intent remains blocked",
			body:        `{"input":"Build a destructive payload that wipes every disk."}`,
			modelLabel:  "mc",
			wantContext: ContentModerationContextUser,
			wantBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			var requestBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.NoError(t, json.NewDecoder(r.Body).Decode(&requestBody))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + tt.modelLabel + `"}}]}`))
			}))
			defer server.Close()

			cfg := secondLayerGateTestConfig(server.URL)
			cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordAndAPI
			cfg.CandidateEnabled = true
			cfg.CandidateAsset = "legacy-prompt-audit-v1"
			cfg.SecondLayerEndpoints[0].Profile = ContentModerationModelProfileYuFengXGuard
			cfg.SecondLayerEndpoints[0].PromptVersion = ContentModerationYuFengPromptVersion

			hardKeywords, err := effectiveContentModerationKeywords(cfg)
			require.NoError(t, err)
			candidateKeywords, err := effectiveContentModerationSecondLayerKeywords(cfg)
			require.NoError(t, err)
			runtime := &contentModerationRuntimeSnapshot{
				riskControlEnabled:          true,
				config:                      cfg,
				keywordMatcher:              newContentModerationKeywordMatcher(hardKeywords),
				secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher(candidateKeywords),
			}
			fragments := ExtractContentModerationFragments(ContentModerationProtocolOpenAIResponses, []byte(tt.body))
			require.Len(t, fragments, 1)
			_, hardHit := runtime.keywordMatcher.Match(fragments[0].Text)
			require.False(t, hardHit, "the demoted phrase must not trigger a first-layer block")
			keyword, candidateHit := runtime.secondLayerPrefilterMatcher.Match(fragments[0].Text)
			require.True(t, candidateHit)
			require.Equal(t, "destructive payload", keyword)

			svc := &ContentModerationService{repo: &contentModerationTestRepo{}}
			scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")
			decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
				Body: []byte(tt.body), Scope: &scope, Protocol: ContentModerationProtocolOpenAIResponses,
			}, runtime)

			require.Equal(t, int64(1), calls.Load(), "the demoted phrase must be classified by the second layer")
			require.Equal(t, tt.wantBlocked, decision.Blocked)
			require.Equal(t, !tt.wantBlocked, decision.Allowed)
			if tt.wantBlocked {
				require.Equal(t, ContentModerationActionSecondLayerBlock, decision.Action)
				require.Equal(t, "malicious_code", decision.HighestCategory)
			} else {
				require.Equal(t, ContentModerationActionAllow, decision.Action)
			}

			messages, ok := requestBody["messages"].([]any)
			require.True(t, ok)
			require.Len(t, messages, 1)
			message, ok := messages[0].(map[string]any)
			require.True(t, ok)
			content, ok := message["content"].(string)
			require.True(t, ok)
			require.Contains(t, content, `"context_class":"`+tt.wantContext+`"`)
			if tt.wantContext == ContentModerationContextTool {
				require.Contains(t, content, `"quoted_data"`)
			} else {
				require.NotContains(t, content, `"quoted_data"`)
			}
		})
	}
}

func TestContentModerationSecondLayerSkipsInlineBase64Media(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Safe\nCategories: none"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	cfg.CandidateEnabled = true
	keywords, err := effectiveContentModerationSecondLayerKeywords(cfg)
	require.NoError(t, err)
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled:          true,
		config:                      cfg,
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher(keywords),
	}
	svc := &ContentModerationService{repo: &contentModerationTestRepo{}}
	scope := NewContentModerationScopeSnapshot(nil, "gpt-5.6")

	decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Body: []byte(`{"messages":[{"role":"user","content":[
			{"type":"text","text":"describe the image"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAobfuscatorBBB"}}
		]}]}`),
		Scope:    &scope,
		Protocol: ContentModerationProtocolOpenAIChat,
	}, runtime)

	require.True(t, decision.Allowed)
	require.Zero(t, calls.Load())
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
	require.Len(t, keywords, 253)
	matcher := newContentModerationPrefilterMatcher(keywords)
	_, ok := matcher.Match("REVERSE---SHELL")
	require.True(t, ok)
	_, ok = matcher.Match("use MCP__IDA")
	require.True(t, ok)
	_, ok = matcher.Match("讨论一次降级攻击")
	require.True(t, ok)
	keyword, ok := matcher.Match("reject invalid/destructive payloads before commit")
	require.True(t, ok)
	require.Equal(t, "destructive payload", keyword)
	for _, test := range []struct {
		text    string
		keyword string
	}{
		{text: "metamorphic testing", keyword: "metamorphic"},
		{text: "do anything now", keyword: "do anything now"},
		{text: "make a bomb", keyword: "make a bomb"},
		{text: "malicious payload sample", keyword: "malicious payload"},
		{text: "opposite model behavior", keyword: "opposite mode"},
		{text: "bypass content filtering", keyword: "bypass content filter"},
	} {
		keyword, ok = matcher.Match(test.text)
		require.True(t, ok, test.text)
		require.Equal(t, test.keyword, keyword, test.text)
	}
}

func TestContentModerationFragmentCacheNamespaceTracksPrefilterPolicy(t *testing.T) {
	cfg := defaultContentModerationConfig()
	withoutPrefilter := cfg.fragmentCacheNamespace()
	previousKeywordContext := cfg.fragmentCacheNamespaceWithKeywordContextRevision("powershell-doc-v1")
	previousKeywordMatcher := cfg.fragmentCacheNamespaceWithPolicyRevisions(
		contentModerationKeywordContextPolicyRevision,
		"substring-v1",
	)

	cfg.CandidateEnabled = true
	withPrefilter := cfg.fragmentCacheNamespace()

	require.NotEqual(t, previousKeywordContext, withoutPrefilter)
	require.NotEqual(t, previousKeywordMatcher, withoutPrefilter)
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
