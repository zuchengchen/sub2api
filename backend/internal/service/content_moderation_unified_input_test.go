package service

import (
	"context"
	"errors"
	"net/http"
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

func TestIsGPTContentModerationGroup(t *testing.T) {
	for _, name := range []string{"GPT", "gpt-prod", "\u3000 GpT Team \t", "GPTanything"} {
		require.True(t, IsGPTContentModerationGroup(name), name)
	}
	for _, name := range []string{"", "GP", "xGPT", "\u0262PT", "ChatGPT"} {
		require.False(t, IsGPTContentModerationGroup(name), name)
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
