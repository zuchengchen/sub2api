package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type contentModerationMetricCache struct {
	contentModerationTestHashCache
	result string
	found  bool
	getErr error
	putErr error
}

type contentModerationFragmentMapCache struct {
	contentModerationTestHashCache
	results map[string]string
}

func (c *contentModerationFragmentMapCache) GetFragmentResult(_ context.Context, namespace, hash string) (string, bool, error) {
	result, found := c.results[namespace+":"+hash]
	return result, found, nil
}

func (c *contentModerationFragmentMapCache) PutFragmentResult(_ context.Context, namespace, hash, result string, _ int64, _ int, _ int64) error {
	if c.results == nil {
		c.results = make(map[string]string)
	}
	c.results[namespace+":"+hash] = result
	return nil
}

func (c *contentModerationFragmentMapCache) DeleteFragmentResult(_ context.Context, namespace, hash string) (bool, error) {
	key := namespace + ":" + hash
	_, found := c.results[key]
	delete(c.results, key)
	return found, nil
}

func (c *contentModerationFragmentMapCache) ClearFragmentResults(_ context.Context, namespace string) (int64, error) {
	prefix := namespace + ":"
	var deleted int64
	for key := range c.results {
		if strings.HasPrefix(key, prefix) {
			delete(c.results, key)
			deleted++
		}
	}
	return deleted, nil
}

func (c *contentModerationFragmentMapCache) CountFragmentResults(_ context.Context, namespace string) (int64, error) {
	prefix := namespace + ":"
	var count int64
	for key := range c.results {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	return count, nil
}

func (c *contentModerationMetricCache) GetFragmentResult(context.Context, string, string) (string, bool, error) {
	return c.result, c.found, c.getErr
}

func (c *contentModerationMetricCache) PutFragmentResult(context.Context, string, string, string, int64, int, int64) error {
	return c.putErr
}

func (*contentModerationMetricCache) DeleteFragmentResult(context.Context, string, string) (bool, error) {
	return false, nil
}

func (*contentModerationMetricCache) ClearFragmentResults(context.Context, string) (int64, error) {
	return 0, nil
}

func (*contentModerationMetricCache) CountFragmentResults(context.Context, string) (int64, error) {
	return 0, nil
}

func TestContentModerationScopeSnapshotDoesNotUseGroupName(t *testing.T) {
	for _, name := range []string{"GPT", "gpt-prod", "\u3000 GpT Team \t", "Claude production", "grok", "", "ChatGPT"} {
		scope := NewContentModerationScopeSnapshot(nil, name)
		require.True(t, scope.InScope, name)
	}
}

func TestContentModerationConfiguredScope(t *testing.T) {
	groupID := int64(41)
	scope := NewContentModerationScopeSnapshot(&groupID, "Claude production")
	baseConfig := defaultContentModerationConfig()

	tests := []struct {
		name        string
		allGroups   bool
		groupIDs    []int64
		modelFilter ContentModerationModelFilter
		model       string
		want        bool
	}{
		{
			name: "all groups and models", allGroups: true,
			modelFilter: ContentModerationModelFilter{Type: ContentModerationModelFilterAll},
			model:       "claude-opus-5", want: true,
		},
		{
			name: "selected non GPT group", groupIDs: []int64{41},
			modelFilter: ContentModerationModelFilter{Type: ContentModerationModelFilterAll},
			model:       "claude-opus-5", want: true,
		},
		{
			name: "unselected group", groupIDs: []int64{99},
			modelFilter: ContentModerationModelFilter{Type: ContentModerationModelFilterAll},
			model:       "claude-opus-5", want: false,
		},
		{
			name: "included model", allGroups: true,
			modelFilter: ContentModerationModelFilter{Type: ContentModerationModelFilterInclude, Models: []string{"Claude-Opus-5"}},
			model:       "claude-opus-5", want: true,
		},
		{
			name: "excluded model", allGroups: true,
			modelFilter: ContentModerationModelFilter{Type: ContentModerationModelFilterExclude, Models: []string{"claude-opus-5"}},
			model:       "CLAUDE-OPUS-5", want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := cloneContentModerationConfig(baseConfig)
			cfg.AllGroups = tt.allGroups
			cfg.GroupIDs = tt.groupIDs
			cfg.ModelFilter = tt.modelFilter
			require.Equal(t, tt.want, cfg.includesGroup(scope.GroupID) && cfg.includesModel(tt.model))
		})
	}
}

func TestExtractContentModerationFragments_AllClientControlledRolesAndReferences(t *testing.T) {
	body := []byte(`{
		"system":[{"type":"text","text":"system text"}],
		"instructions":"developer instructions",
		"messages":[
			{"role":"user","content":[{"type":"input_text","text":"user text"},{"type":"input_file","filename":"brief.txt","file_url":"https://files.example/brief.txt"}]},
			{"role":"assistant","content":"assistant text"},
			{"role":"developer","content":"developer text"},
			{"role":"tool","content":{"type":"function_call_output","output":"tool output","url":"https://tool.example/result"}}
		],
		"tools":[{"type":"function","name":"dangerous_tool","description":"tool description"}]
	}`)

	fragments := ExtractContentModerationFragments(ContentModerationProtocolOpenAIResponses, body)
	texts := make(map[string]ContentModerationFragment, len(fragments))
	for _, fragment := range fragments {
		texts[fragment.Text] = fragment
		require.Len(t, fragment.Hash, 64)
	}
	for _, text := range []string{
		"system text", "developer instructions", "user text", "brief.txt",
		"https://files.example/brief.txt", "assistant text", "developer text",
		"tool output", "https://tool.example/result", "dangerous_tool", "tool description",
	} {
		require.Contains(t, texts, text)
	}
	require.Equal(t, "system", texts["system text"].Role)
	require.Equal(t, "assistant", texts["assistant text"].Role)
	require.Equal(t, "tool", texts["tool output"].Role)
	require.Equal(t, "file", texts["brief.txt"].Kind)
	require.Equal(t, "url", texts["https://files.example/brief.txt"].Kind)
}

func TestSelectContentModerationReviewFragments_AllUserTurnsAndLatestTools(t *testing.T) {
	body := []byte(`{
		"system":"system policy",
		"messages":[
			{"role":"user","content":"old user text"},
			{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"old_call","arguments":"old tool arguments"}}]},
			{"role":"tool","content":"old tool output"},
			{"role":"user","content":[{"type":"text","text":"latest user text"},{"type":"input_file","filename":"photo.png","file_url":"https://media.example/photo.png"}]},
			{"role":"assistant","content":"assistant prose","tool_calls":[{"type":"function","function":{"name":"new_call","arguments":"related tool arguments"}}]},
			{"role":"tool","content":"related tool output"}
		],
		"tools":[{"type":"function","function":{"name":"schema_only","description":"tool schema description"}}]
	}`)

	fragments := SelectContentModerationReviewFragments(ExtractContentModerationFragments(ContentModerationProtocolOpenAIChat, body))
	texts := make(map[string]ContentModerationFragment, len(fragments))
	for _, fragment := range fragments {
		texts[fragment.Text] = fragment
	}
	// Every user-authored turn is reviewable: earlier turns are client
	// content too and must not become a smuggling channel.
	for _, expected := range []string{"old user text", "latest user text", "photo.png", "new_call", "related tool arguments", "related tool output"} {
		require.Contains(t, texts, expected)
	}
	for _, excluded := range []string{
		"system policy", "old_call", "old tool arguments", "old tool output",
		"assistant prose", "https://media.example/photo.png", "schema_only", "tool schema description",
	} {
		require.NotContains(t, texts, excluded)
	}
	require.Equal(t, "user", texts["old user text"].Role)
	require.Equal(t, "tool", texts["related tool arguments"].Role)
}

func TestSelectContentModerationReviewFragments_ToolOnlyContinuation(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call_output","call_id":"call_1","output":"current tool result"},
		{"type":"input_image","image_url":"https://media.example/output.png"}
	]}`)
	fragments := SelectContentModerationReviewFragments(ExtractContentModerationFragments(ContentModerationProtocolOpenAIResponses, body))
	require.Len(t, fragments, 1)
	require.Equal(t, "tool", fragments[0].Role)
	require.Equal(t, "current tool result", fragments[0].Text)
}

func TestContentModerationFragmentHashSeparatesRoleKindPathAndContext(t *testing.T) {
	toolText, ok := newContentModerationFragment(" Tool ", " Text ", "messages.1.content", "same text")
	require.True(t, ok)
	samePath, ok := newContentModerationFragment("tool", "text", "  messages.1.content  ", "same text")
	require.True(t, ok)
	differentPath, ok := newContentModerationFragment("tool", "text", "messages.9.content", "same text")
	require.True(t, ok)
	userText, ok := newContentModerationFragment("user", "text", "messages.1.content", "same text")
	require.True(t, ok)
	toolFile, ok := newContentModerationFragment("tool", "file", "messages.1.content", "same text")
	require.True(t, ok)

	require.Equal(t, toolText.Hash, samePath.Hash, "normalized metadata must keep a stable hash")
	require.NotEqual(t, toolText.Hash, differentPath.Hash)
	require.NotEqual(t, toolText.Hash, userText.Hash)
	require.NotEqual(t, toolText.Hash, toolFile.Hash)
}

func TestUnifiedModerationAllowsOnlyPlaceholderEncodedCommandInToolMarkdown(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordOnly
	cfg.FirstLayerStage = ContentModerationFirstLayerStageEnforce
	cfg.SecondLayerEnabled = false
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true,
		config:             cfg,
		keywordMatcher: newContentModerationKeywordMatcher([]string{
			"powershell -enc",
			"powershell -EncodedCommand",
			"write a virus",
		}),
	}
	cache := &contentModerationFragmentMapCache{}
	svc := &ContentModerationService{hashCache: cache, repo: &contentModerationTestRepo{}}
	scope := NewContentModerationScopeSnapshot(nil, "GPT")
	markdown := `<file-view path="C:\work\backlog.md" title="backlog.md">
40|critical turbo=confirm | powershell -EncodedCommand <base64>
90|powershell -EncodedCommand <base64 rm -rf />    → critical turbo=confirm
</file-view>`

	check := func(role, text string) *ContentModerationDecision {
		t.Helper()
		body := []byte(`{"messages":[{"role":"` + role + `","content":` + strconv.Quote(text) + `}]}`)
		return svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
			Body: body, Scope: &scope, Protocol: ContentModerationProtocolOpenAIChat,
		}, runtime)
	}

	fragment, ok := newContentModerationFragment("tool", "text", "messages.0.content", markdown)
	require.True(t, ok)
	oldNamespace := cfg.fragmentCacheNamespaceWithKeywordContextRevision("powershell-doc-v1")
	newNamespace := cfg.fragmentCacheNamespace()
	require.NotEqual(t, oldNamespace, newNamespace)
	cache.results = map[string]string{
		oldNamespace + ":" + fragment.Hash: ContentModerationFragmentBlock,
	}

	require.True(t, check("tool", markdown).Allowed, "an old cached block must not survive a context-policy revision")
	require.Equal(t, ContentModerationFragmentAllow, cache.results[newNamespace+":"+fragment.Hash])
	require.True(t, check("tool", markdown).Allowed, "the corrected allow decision must be reusable from cache")
	require.True(t, check("user", markdown).Blocked, "a tool allow-cache entry must not apply to user input")

	for name, text := range map[string]string{
		"real payload":           strings.Replace(markdown, "<base64>", "SQBFAFgAKABOAGUAdwA=", 1),
		"short real payload":     strings.Replace(strings.Replace(markdown, "-EncodedCommand", "-enc", 1), "<base64>", "SQBFAFgAKABOAGUAdwA=", 1),
		"non-markdown":           strings.Replace(markdown, "backlog.md", "backlog.txt", 2),
		"unclosed placeholder":   strings.Replace(markdown, "<base64>", "<base64", 1),
		"unmarked placeholder":   strings.Replace(markdown, "<base64>", "<documentation>", 1),
		"oversized placeholder":  strings.Replace(markdown, "<base64>", "<base64 "+strings.Repeat("x", maxDocumentationCommandPlaceholderBytes)+">", 1),
		"different risk keyword": strings.Replace(markdown, "</file-view>", "write a virus\n</file-view>", 1),
		"mixed": strings.Replace(markdown, "</file-view>",
			"powershell -EncodedCommand SQBFAFgAKABOAGUAdwA=\n</file-view>", 1),
	} {
		t.Run(name, func(t *testing.T) {
			require.True(t, check("tool", text).Blocked)
		})
	}

	shortFlag := strings.ReplaceAll(markdown, "-EncodedCommand", "-enc")
	require.True(t, check("tool", shortFlag).Allowed, "a short flag followed only by placeholders is documentation")
}

func TestExtractContentModerationFragments_SkipsInlineBase64MediaURLs(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"input_text","text":"describe this image"},
			{"type":"input_image","image_url":"data:image/png;base64,AAAsh3llBBB"},
			{"type":"input_audio","url":"DATA:AUDIO/WAV;CHARSET=binary;BASE64,AAAsp00fBBB"},
			{"type":"input_text","text":"https://images.example/photo.png"}
		]}]
	}`)

	fragments := ExtractContentModerationFragments(ContentModerationProtocolOpenAIResponses, body)
	texts := make([]string, 0, len(fragments))
	for _, fragment := range fragments {
		texts = append(texts, fragment.Text)
	}
	require.Contains(t, texts, "describe this image")
	require.Contains(t, texts, "https://images.example/photo.png")
	require.NotContains(t, texts, "data:image/png;base64,AAAsh3llBBB")
	require.NotContains(t, texts, "DATA:AUDIO/WAV;CHARSET=binary;BASE64,AAAsp00fBBB")
}

func TestUnifiedModerationDoesNotMatchKeywordInsideInlineBase64Media(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordOnly
	cfg.SecondLayerEnabled = false
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true,
		config:             cfg,
		keywordMatcher:     newContentModerationKeywordMatcher([]string{"sh3ll"}),
	}
	svc := &ContentModerationService{repo: &contentModerationTestRepo{}}
	scope := NewContentModerationScopeSnapshot(nil, "GPT")

	decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Body: []byte(`{"messages":[{"role":"user","content":[
			{"type":"text","text":"describe the attached image"},
			{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,AAAsh3llBBB"}}
		]}]}`),
		Scope:    &scope,
		Protocol: ContentModerationProtocolOpenAIChat,
	}, runtime)

	require.True(t, decision.Allowed)
}

func TestContentModerationPendingBodyBudget(t *testing.T) {
	budget := NewContentModerationPendingBodyBudget()
	first, ok := budget.TryReserve(7, 10)
	require.True(t, ok)
	require.True(t, first.Retain())
	_, ok = budget.TryReserve(4, 10)
	require.False(t, ok)
	require.EqualValues(t, 1, budget.Rejections())
	first.Release()
	require.EqualValues(t, 7, budget.InUse())
	first.Release()
	require.Zero(t, budget.InUse())
	require.EqualValues(t, 7, budget.MaxSeen())
}

func TestContentModerationRequestBodyHistogramUsesFixedBuckets(t *testing.T) {
	svc := &ContentModerationService{}
	for _, size := range []int64{0, 64 << 10, (64 << 10) + 1, (1 << 20) + 1, (16 << 20) + 1, (64 << 20) + 1, (256 << 20) + 1} {
		reservation, ok := svc.ReservePendingRequestBody(size)
		require.True(t, ok)
		reservation.Release()
	}

	histogram := svc.contentModerationBodySizeHistogram()
	require.Equal(t, []ContentModerationBodySizeBucket{
		{UpperBoundBytes: 64 << 10, Count: 2},
		{UpperBoundBytes: 1 << 20, Count: 1},
		{UpperBoundBytes: 16 << 20, Count: 1},
		{UpperBoundBytes: 64 << 20, Count: 1},
		{UpperBoundBytes: 256 << 20, Count: 1},
		{UpperBoundBytes: -1, Count: 1},
	}, histogram)
	require.EqualValues(t, (256<<20)+1, svc.observedRequestBodyMax.Load())
}

func TestUnifiedFragmentCacheMetricsAreFixedCounters(t *testing.T) {
	newRuntime := func() *contentModerationRuntimeSnapshot {
		cfg := defaultContentModerationConfig()
		cfg.Enabled = true
		cfg.AutoBanEnabled = false
		cfg.SecondLayerEnabled = false
		return &contentModerationRuntimeSnapshot{riskControlEnabled: true, config: cfg}
	}
	input := ContentModerationCheckInput{
		Body: []byte(`{"messages":[{"role":"user","content":"cache metric text"}]}`),
		Scope: func() *ContentModerationScopeSnapshot {
			scope := NewContentModerationScopeSnapshot(nil, "GPT")
			return &scope
		}(),
	}

	missCache := &contentModerationMetricCache{}
	missSvc := &ContentModerationService{hashCache: missCache, repo: &contentModerationTestRepo{}}
	decision := missSvc.checkUnifiedFragments(context.Background(), input, newRuntime())
	require.True(t, decision.Allowed)
	require.EqualValues(t, 1, missSvc.fragmentCacheMisses.Load())
	require.EqualValues(t, 1, missSvc.fragmentCacheWrites.Load())

	hitCache := &contentModerationMetricCache{result: ContentModerationFragmentAllow, found: true}
	hitSvc := &ContentModerationService{hashCache: hitCache, repo: &contentModerationTestRepo{}}
	decision = hitSvc.checkUnifiedFragments(context.Background(), input, newRuntime())
	require.True(t, decision.Allowed)
	require.EqualValues(t, 1, hitSvc.fragmentCacheHits.Load())
	require.Zero(t, hitSvc.fragmentCacheWrites.Load())

	errorCache := &contentModerationMetricCache{getErr: errors.New("redis unavailable"), putErr: errors.New("redis unavailable")}
	errorSvc := &ContentModerationService{hashCache: errorCache, repo: &contentModerationTestRepo{}}
	decision = errorSvc.checkUnifiedFragments(context.Background(), input, newRuntime())
	require.True(t, decision.Allowed)
	require.EqualValues(t, 1, errorSvc.fragmentCacheErrors.Load())
	require.EqualValues(t, 1, errorSvc.fragmentCacheWriteErrors.Load())
	require.NotContains(t, strings.ToLower(decision.Message), "redis")
}

func TestUnifiedFragmentCacheBlockPreservesMatchedKeyword(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordAndAPI
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true,
		config:             cfg,
		keywordMatcher:     newContentModerationKeywordMatcher([]string{"secret-token"}),
	}
	cache := &contentModerationMetricCache{result: ContentModerationFragmentBlock, found: true}
	svc := &ContentModerationService{hashCache: cache, repo: &contentModerationTestRepo{}}
	scope := NewContentModerationScopeSnapshot(nil, "GPT")

	decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Body:     []byte(`{"messages":[{"role":"user","content":"leak SECRET-TOKEN"}]}`),
		Scope:    &scope,
		Protocol: ContentModerationProtocolOpenAIChat,
	}, runtime)

	require.True(t, decision.Blocked)
	require.Equal(t, ContentModerationActionCacheBlock, decision.Action)
	require.Equal(t, contentModerationKeywordCategory, decision.HighestCategory)
	require.Equal(t, "secret-token", decision.MatchedKeyword)
}

func TestUnifiedFragmentCacheBlockDoesNotReportDisabledKeywordLayer(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.KeywordBlockingMode = ContentModerationKeywordModeAPIOnly
	runtime := &contentModerationRuntimeSnapshot{
		riskControlEnabled: true,
		config:             cfg,
		keywordMatcher:     newContentModerationKeywordMatcher([]string{"secret-token"}),
	}
	cache := &contentModerationMetricCache{result: ContentModerationFragmentBlock, found: true}
	svc := &ContentModerationService{hashCache: cache, repo: &contentModerationTestRepo{}}
	scope := NewContentModerationScopeSnapshot(nil, "GPT")

	decision := svc.checkUnifiedFragments(context.Background(), ContentModerationCheckInput{
		Body:     []byte(`{"messages":[{"role":"user","content":"leak SECRET-TOKEN"}]}`),
		Scope:    &scope,
		Protocol: ContentModerationProtocolOpenAIChat,
	}, runtime)

	require.True(t, decision.Blocked)
	require.Equal(t, ContentModerationActionCacheBlock, decision.Action)
	require.Equal(t, "fragment_cache", decision.HighestCategory)
	require.Empty(t, decision.MatchedKeyword)
}

func TestContentModerationRawRequestCloneMetadataPreservesRepeatedHeaders(t *testing.T) {
	raw := ContentModerationRawRequest{Headers: http.Header{"X-Test": {"one", "two"}}, Body: []byte("raw")}
	clone := raw.CloneMetadata()
	clone.Headers.Add("X-Test", "three")
	require.Equal(t, []string{"one", "two"}, raw.Headers.Values("X-Test"))
	require.Equal(t, []byte("raw"), clone.Body)
}
