//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestOAuthReliabilityCodexModelsCold304IsRetryableProtocolFailure(t *testing.T) {
	for _, standard := range []bool{false, true} {
		t.Run(fmt.Sprintf("standard=%t", standard), func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			request := openAIModelsRequest{accountID: 7, url: "https://synthetic.example/models", standardModelsList: standard}
			calls := 0
			fetch := func(context.Context, string) (*OpenAIModelsResponse, error) {
				calls++
				return &OpenAIModelsResponse{NotModified: true, ETag: `"unexpected"`}, nil
			}
			for attempt := 0; attempt < 2; attempt++ {
				manifest, err := svc.fetchCachedOpenAIModels(context.Background(), request, fetch, `"untrusted-client-etag"`)
				require.Nil(t, manifest)
				require.Error(t, err)
				require.Equal(t, http.StatusBadGateway, infraerrors.Code(err))
				require.Equal(t, "OPENAI_MODELS_UNEXPECTED_NOT_MODIFIED", infraerrors.Reason(err))
				require.True(t, IsRetryableCodexModelsManifestError(err))
				stage, class := CodexModelsFailureMetadata(err, CodexModelsStageUpstreamFetch)
				require.Equal(t, "cache_revalidation", stage)
				require.Equal(t, "upstream_protocol", class)
			}
			require.Equal(t, 2, calls, "an invalid cold304 must never populate the cache")
			_, state := svc.openAIModelsCache.get(buildOpenAIModelsCacheKey(request), time.Now())
			require.Equal(t, openAIModelsCacheMiss, state)
		})
	}
}

func TestOAuthReliabilityCodexModelsWarm304KeepsCurrentGroupFiltering(t *testing.T) {
	const groupID int64 = 741
	svc := &OpenAIGatewayService{accountRepo: codexModelsVisibilityAccountRepo{}}
	request := openAIModelsRequest{accountID: 7, url: "https://synthetic.example/models"}
	key := buildOpenAIModelsCacheKey(request)
	raw := []byte(`{"models":[{"slug":"allowed"},{"slug":"hidden"}]}`)
	seed := &OpenAIModelsResponse{Body: raw, ETag: `"raw"`, upstreamETag: `"source"`}
	svc.openAIModelsCache.set(key, seed, time.Now().Add(-openAIModelsCacheTTL-time.Second))
	result := <-svc.refreshCachedOpenAIModels(key, request, func(_ context.Context, etag string) (*OpenAIModelsResponse, error) {
		require.Equal(t, `"source"`, etag)
		return &OpenAIModelsResponse{NotModified: true, ETag: `"source"`}, nil
	})
	require.NoError(t, result.Err)
	manifest, ok := result.Val.(*OpenAIModelsResponse)
	require.True(t, ok)
	require.Equal(t, raw, manifest.Body)
	group := &Group{ID: groupID, Platform: PlatformOpenAI, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"allowed"}}}
	first := openAIModelsResponseForClient(manifest, "")
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(context.Background(), group, first, ""))
	require.Contains(t, string(first.Body), `"allowed"`)
	require.NotContains(t, string(first.Body), `"hidden"`)
	firstETag := first.ETag
	group.ModelAllowlist.Models = []string{"hidden"}
	second := openAIModelsResponseForClient(manifest, "")
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(context.Background(), group, second, firstETag))
	require.False(t, second.NotModified)
	require.NotEqual(t, firstETag, second.ETag)
	require.NotContains(t, string(second.Body), `"allowed"`)
	require.Contains(t, string(second.Body), `"hidden"`)
	third := openAIModelsResponseForClient(manifest, "")
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(context.Background(), group, third, second.ETag))
	require.True(t, third.NotModified)
	cached, state := svc.openAIModelsCache.get(key, time.Now())
	require.Equal(t, openAIModelsCacheFresh, state)
	require.Equal(t, raw, cached.Body, "a group-specific filtered body must not replace the shared raw cache")
}

func TestOAuthReliabilityCodexManifestFailureMetadataIsBounded(t *testing.T) {
	secret := errors.New("synthetic-secret-provider-body")
	repo := &countingCodexModelsAccountRepo{err: secret}
	svc := &OpenAIGatewayService{accountRepo: repo}
	_, _, err := svc.BuildGroupConfiguredCodexModelsManifest(context.Background(), &Group{ID: 742, Platform: PlatformOpenAI}, "")
	require.ErrorIs(t, err, secret)
	stage, class := CodexModelsFailureMetadata(err, CodexModelsStageUnknown)
	require.Equal(t, "group_account_read", stage)
	require.Equal(t, "repository", class)
	require.NotContains(t, err.Error(), "synthetic-secret")
	svc.accountRepo = codexModelsVisibilityAccountRepo{}
	err = svc.MergeGroupConfiguredCodexModels(context.Background(), &Group{ID: 742, Platform: PlatformOpenAI}, &OpenAIModelsResponse{Body: []byte(`{"models":"synthetic-secret-provider-body"}`)}, "")
	require.Error(t, err)
	stage, class = CodexModelsFailureMetadata(err, CodexModelsStageUnknown)
	require.Equal(t, "configured_merge", stage)
	require.Equal(t, "invalid_shape", class)
	for _, tc := range []struct {
		err   error
		class string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline"},
		{&json.SyntaxError{Offset: 1}, "invalid_json"},
		{secret, "unknown"},
	} {
		stage, class = CodexModelsFailureMetadata(tc.err, CodexModelsFailureStage("synthetic-secret-stage"))
		require.Equal(t, "unknown", stage)
		require.Equal(t, tc.class, class)
	}
}

func TestOAuthReliabilityCodexModelsCanceledBeforeColdFetchDoesNotStartUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc := &OpenAIGatewayService{}
	called := false
	manifest, err := svc.fetchCachedOpenAIModels(ctx, openAIModelsRequest{}, func(context.Context, string) (*OpenAIModelsResponse, error) {
		called = true
		return nil, errors.New("unexpected")
	}, "")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, manifest)
	require.False(t, called)
}
