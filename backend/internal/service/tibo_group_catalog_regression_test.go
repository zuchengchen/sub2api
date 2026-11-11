package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTiboHealthyGroupModelAuthCacheNameRoundTrip(t *testing.T) {
	for _, name := range []string{TiboHealthyGroupName, TiboWelfareGroupName, "other-openai"} {
		t.Run(name, func(t *testing.T) {
			groupID := int64(8751)
			apiKey := &APIKey{
				ID: 8752, UserID: 8753, GroupID: &groupID, Key: "test-tibo-cache-key", Status: StatusActive,
				User: &User{ID: 8753, Role: RoleUser, Status: StatusActive},
				Group: &Group{
					ID: groupID, Name: name, Platform: PlatformOpenAI, Status: StatusActive,
					ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-luna"}},
				},
			}
			svc := &APIKeyService{}
			entry := APIKeyAuthCacheEntry{Snapshot: svc.snapshotFromAPIKey(context.Background(), apiKey)}
			require.NotNil(t, entry.Snapshot)
			require.NotNil(t, entry.Snapshot.Group)
			require.Equal(t, name, entry.Snapshot.Group.Name)
			encoded, err := json.Marshal(entry)
			require.NoError(t, err)
			var cached APIKeyAuthCacheEntry
			require.NoError(t, json.Unmarshal(encoded, &cached))
			restored, hit, err := svc.applyAuthCacheEntry(apiKey.Key, &cached)
			require.NoError(t, err)
			require.True(t, hit)
			require.NotNil(t, restored)
			require.NotNil(t, restored.Group)
			require.Equal(t, name, restored.Group.Name)
			require.Equal(t, PlatformOpenAI, restored.Group.Platform)
			require.Equal(t, apiKey.Group.ModelAllowlist, restored.Group.ModelAllowlist,
				"auth snapshots must persist the configured settings, not the derived policy")
			policy := restored.Group.EffectiveModelAllowlist()
			healthy := name == TiboHealthyGroupName
			require.True(t, policy.Allows("gpt-6-luna"))
			require.Equal(t, healthy, policy.Allows("gpt-6-sol"))
		})
	}
}

func TestTiboHealthyGroupModelSharedSourceCacheFreshStaleAndRevalidation(t *testing.T) {
	// Exercise both account cache routes. API-key refreshes use the source ETag
	// and can return 304; OAuth refreshes follow the unconditional Codex route.
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		t.Run(accountType, func(t *testing.T) {
			const sourceBody = `{"models":[{"slug":"gpt-6-luna"},{"slug":"gpt-5.6-luna"},{"slug":"luna"},{"slug":"gpt-6-sol","retained_metadata":{"context":424242}},{"slug":"gpt-6-astra"},{"slug":"gpt-6.1-terra"}],"source_marker":"shared"}`
			const sourceETag = `"tibo-source-v1"`
			account := newCodexModelsAPIKeyTestAccount("https://tibo-source.example/v1")
			if accountType == AccountTypeOAuth {
				account = newCodexModelsTestAccount()
			}
			account.ID = 8761
			account.Status, account.Schedulable = StatusActive, true
			var calls atomic.Int32
			refreshStarted := make(chan string, 1)
			releaseRefresh := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseRefresh) }) }
			t.Cleanup(release)
			upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
				if accountID != account.ID {
					t.Errorf("source request selected account %d, want %d", accountID, account.ID)
				}
				call := calls.Add(1)
				headers := make(http.Header)
				headers.Set("ETag", sourceETag)
				if call > 1 {
					if call == 2 {
						refreshStarted <- req.Header.Get("If-None-Match")
					}
					select {
					case <-releaseRefresh:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
					if accountType == AccountTypeAPIKey {
						return &http.Response{StatusCode: http.StatusNotModified, Header: headers, Body: http.NoBody}, nil
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(sourceBody))}, nil
			}}
			svc := newCodexModelsAPIKeyTestService(upstream)
			healthy := &Group{
				ID: 8762, Name: TiboHealthyGroupName, Platform: PlatformOpenAI,
				ModelAllowlist:            GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-luna"}},
				CodexModelsManifestConfig: GroupCodexModelsManifestConfig{Enabled: true},
			}
			other := &Group{ID: 8763, Name: TiboWelfareGroupName, Platform: PlatformOpenAI}
			svc.accountRepo = codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{
				healthy.ID: {*account}, other.ID: {*account},
			}}
			fetchForGroup := func(group *Group, clientETag string) *OpenAIModelsResponse {
				t.Helper()
				// The handler deliberately fetches an unconditional source before
				// comparing the final group-specific client ETag.
				manifest, err := svc.FetchCodexModelsManifest(context.Background(), account, "0.144.0", "")
				require.NoError(t, err)
				require.False(t, manifest.NotModified)
				require.NoError(t, ApplyPinnedCodexModelsMapping(manifest, account, group))
				require.NoError(t, svc.MergeGroupConfiguredCodexModels(context.Background(), group, manifest, clientETag))
				return manifest
			}
			assertHealthy := func(manifest *OpenAIModelsResponse) {
				t.Helper()
				require.False(t, manifest.NotModified)
				require.ElementsMatch(t, []string{"gpt-6-luna", "gpt-5.6-luna", "luna", "gpt-6-sol", "gpt-6-astra", "gpt-6.1-terra"}, codexManifestModelSlugs(t, manifest.Body))
				require.Equal(t, int64(424242), gjson.GetBytes(manifest.Body, `models.#(slug=="gpt-6-sol").retained_metadata.context`).Int())
				require.Equal(t, "shared", gjson.GetBytes(manifest.Body, "source_marker").String())
				require.Equal(t, CodexModelsManifestETag(manifest.Body), manifest.ETag)
			}
			firstOther := fetchForGroup(other, "")
			require.ElementsMatch(t, []string{"gpt-6-luna", "gpt-5.6-luna", "luna", "gpt-6-sol", "gpt-6-astra", "gpt-6.1-terra"}, codexManifestModelSlugs(t, firstOther.Body))
			firstHealthy := fetchForGroup(healthy, "")
			assertHealthy(firstHealthy)
			require.JSONEq(t, string(firstOther.Body), string(firstHealthy.Body))
			require.NotEqual(t, firstOther.ETag, firstHealthy.ETag, "the upstream ETag is distinct from the serialized group catalog hash")
			require.False(t, fetchForGroup(healthy, firstOther.ETag).NotModified)
			unchanged := fetchForGroup(healthy, firstHealthy.ETag)
			require.True(t, unchanged.NotModified)
			require.Empty(t, unchanged.Body)
			require.EqualValues(t, 1, calls.Load(), "both groups must reuse the same fresh account cache")

			expireCodexModelsManifestCache(svc, time.Minute)
			staleHealthy := fetchForGroup(healthy, "")
			assertHealthy(staleHealthy)
			require.Equal(t, firstHealthy.ETag, staleHealthy.ETag)
			select {
			case conditionalETag := <-refreshStarted:
				if accountType == AccountTypeAPIKey {
					require.Equal(t, sourceETag, conditionalETag, "upstream revalidation must never use the group ETag")
				} else {
					require.Empty(t, conditionalETag, "OAuth discovery must keep its unconditional request contract")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stale account catalog did not start its background refresh")
			}
			release()
			require.Eventually(t, func() bool {
				svc.openAIModelsCache.mu.Lock()
				defer svc.openAIModelsCache.mu.Unlock()
				if len(svc.openAIModelsCache.entries) != 1 {
					return false
				}
				for _, entry := range svc.openAIModelsCache.entries {
					return time.Now().Before(entry.expiresAt)
				}
				return false
			}, 2*time.Second, 5*time.Millisecond, "refresh must renew the cached source")
			assertHealthy(fetchForGroup(healthy, ""))
			require.True(t, fetchForGroup(healthy, firstHealthy.ETag).NotModified)
			otherAgain := fetchForGroup(other, "")
			require.Equal(t, firstOther.ETag, otherAgain.ETag)
			require.JSONEq(t, string(firstOther.Body), string(otherAgain.Body), "group filtering must leave the cached source reusable")
			require.True(t, fetchForGroup(other, firstOther.ETag).NotModified)
			require.EqualValues(t, 2, calls.Load(), "stale refresh must be singleflight and subsequent requests must remain cached")
		})
	}
}

func TestTiboHealthyGroupModelInternalImageDriverReachesUpstream(t *testing.T) {
	t.Setenv("SUB2API_IMAGES_MAIN_MODEL", "")
	for _, tc := range []struct {
		name, model string
		responses   bool
		fallback    bool
	}{
		{name: "images", model: "gpt-image-1"},
		{name: "responses", model: "gpt-image-1", responses: true},
		{name: "direct-images-fallback", model: "gpt-image-2", fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &Group{
				ID: 8771, Name: TiboHealthyGroupName, Platform: PlatformOpenAI, AllowImageGeneration: true,
				ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-luna"}},
			}
			require.True(t, group.EffectiveModelAllowlist().Allows(tc.model))
			require.True(t, group.EffectiveModelAllowlist().Allows("gpt-5.6-luna"))
			body := []byte(`{"model":"` + tc.model + `","prompt":"draw a cat","input":"draw a cat","stream":false}`)
			c, recorder := newOpenAIImagesTestContext(t, body)
			c.Set("api_key", &APIKey{ID: 8772, GroupID: &group.ID, Group: group})
			response := &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_tibo_image\",\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aW1hZ2U=\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")),
			}
			upstream := &httpUpstreamRecorder{resp: response}
			if tc.fallback {
				upstream.responses = []*http.Response{
					{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"endpoint unavailable"}}`))},
					response,
				}
			}
			svc := newOpenAIImagesTestService(upstream)
			svc.tiboRouteDisabled = true
			account := directImagesTestAccount()
			var result *OpenAIForwardResult
			var err error
			if tc.responses {
				c.Request.URL.Path = "/v1/responses"
				result, err = svc.Forward(context.Background(), c, account, body)
			} else {
				parsed, parseErr := svc.ParseOpenAIImagesRequest(c, body)
				require.NoError(t, parseErr)
				result, err = svc.ForwardImages(context.Background(), c, account, body, parsed, "")
			}
			require.NoError(t, err, recorder.Body.String())
			require.NotNil(t, result)
			require.NotNil(t, upstream.lastReq, "positive image admission must reach the upstream transport")
			require.Equal(t, "/backend-api/codex/responses", upstream.lastReq.URL.Path)
			require.Equal(t, "gpt-5.6-luna", gjson.GetBytes(upstream.lastBody, "model").String())
			require.Equal(t, "image_generation", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
			require.Equal(t, tc.model, gjson.GetBytes(upstream.lastBody, "tools.0.model").String())
			require.Contains(t, recorder.Body.String(), "aW1hZ2U=")
			require.Equal(t, tc.model, gjson.GetBytes(body, "model").String(), "the client model must remain distinct from the internal driver")
			if tc.fallback {
				require.Len(t, upstream.requests, 2)
				require.Equal(t, "/backend-api/codex/images/generations", upstream.requests[0].URL.Path)
				require.Equal(t, tc.model, gjson.GetBytes(upstream.bodies[0], "model").String())
			} else {
				require.Len(t, upstream.requests, 1)
			}
		})
	}
}
