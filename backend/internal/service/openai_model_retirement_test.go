package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSparkRetirementAliasesAndMappings(t *testing.T) {
	aliases := []string{
		"gpt-5.3-codex-spark", "openai/GPT5.3CodexSpark", "GPT_5.3_CODEX_SPARK",
		"gpt 5.3 codex spark", "gpt-5.3-codexspark-high", "gpt5.3codexspark-xhigh-openai-compact",
		"gpt-5.3-codex-spark-2026-02-12", "gpt-5.3-codex-spark-openai-compact",
	}
	for _, typ := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey} {
		for _, passthrough := range []bool{false, true} {
			account := &Account{Platform: PlatformOpenAI, Type: typ, Extra: map[string]any{"openai_passthrough": passthrough}}
			for _, model := range aliases {
				require.True(t, openai.IsRetiredSparkModel(model), model)
				require.False(t, account.IsModelSupported(model), "%s passthrough=%v model=%s", typ, passthrough, model)
				require.True(t, openai.IsRetiredSparkModel(normalizeOpenAIModelForUpstream(account, model)), "normalization must retain retired identity: %s", model)
				for _, mapping := range []map[string]any{{model: "gpt-5.4"}, {"legacy*": model}} {
					account.Credentials = map[string]any{"model_mapping": mapping}
					require.False(t, account.IsModelSupported(model), "mapping source must not revive Spark")
					if !passthrough {
						require.False(t, account.IsModelSupported("legacy-client"), "mapped target must not revive Spark")
					}
				}
				account.Credentials = map[string]any{"compact_model_mapping": map[string]any{"custom*": model}}
				require.False(t, supportsOpenAIRequestModel(account, "custom-model", true))
				account.Credentials = nil
			}
			for _, model := range []string{"custom-model", "muse-spark", "iFlytek/spark", "gpt-5.3-codex", "gpt-5.3-codex-sparkle"} {
				require.False(t, openai.IsRetiredSparkModel(model), model)
				require.True(t, account.IsModelSupported(model), model)
			}
		}
	}
}

func TestSparkRetirementRejectsBeforeUpstreamOrFallback(t *testing.T) {
	for _, typ := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, passthrough := range []bool{false, true} {
			for _, endpoint := range []string{"responses", "chat", "messages", "compact", "probe"} {
				t.Run(typ+"/"+endpoint+"/"+map[bool]string{true: "passthrough", false: "normal"}[passthrough], func(t *testing.T) {
					var calls atomic.Int32
					upstream := &codexModelsHTTPUpstreamStub{do: func(*http.Request, string, int64, int) (*http.Response, error) {
						calls.Add(1)
						return ordinaryModelsUpstreamResponse(`{}`), nil
					}}
					svc := newCodexModelsAPIKeyTestService(upstream)
					svc.cfg.Gateway.OpenAICompactModel = "gpt-5.4"
					account := &Account{ID: 1, Platform: PlatformOpenAI, Type: typ, Extra: map[string]any{"openai_passthrough": passthrough}}
					body := []byte(`{"model":"openai/gpt5.3codexspark-xhigh","input":[],"messages":[],"instructions":"test"}`)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					var err error
					switch endpoint {
					case "responses":
						_, err = svc.Forward(context.Background(), c, account, body)
					case "chat":
						_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
					case "messages":
						_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
					case "compact":
						c.Request.URL.Path = "/v1/responses/compact"
						require.Empty(t, svc.resolveOpenAICompactFallbackModel(account, "gpt-5.3-codex-spark"))
						_, err = svc.Forward(context.Background(), c, account, body)
					case "probe":
						err = (&AccountTestService{httpUpstream: upstream}).testOpenAIAccountConnection(c, account, "gpt-5.3-codex-spark", "", "")
					}
					require.ErrorContains(t, err, "retired")
					var failover *UpstreamFailoverError
					require.NotErrorAs(t, err, &failover)
					require.Zero(t, calls.Load())
				})
			}
		}
		account := &Account{Platform: PlatformOpenAI, Type: typ, Credentials: map[string]any{"model_mapping": map[string]any{"legacy": "gpt-5.3-codex-spark-high"}}}
		require.Error(t, validateRetiredOpenAIRequestModel(account, "legacy", false, ""))
		require.Error(t, validateRetiredOpenAIRequestModel(account, "claude-sonnet", false, "gpt-5.3-codex-spark"))
		account.Credentials = map[string]any{"compact_model_mapping": map[string]any{"gpt-5.4": "gpt5.3codexspark"}}
		svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAICompactModel: "gpt-5.4"}}}
		require.Empty(t, svc.resolveOpenAICompactFallbackModel(account, "gpt-5.4"))
		account.Credentials = nil
		svc.cfg.Gateway.OpenAICompactModel = "gpt-5.3-codex-spark"
		require.Empty(t, svc.resolveOpenAICompactFallbackModel(account, "gpt-5.4"))
	}
}

func TestSparkRetirementMappedForwardDoesNotCallUpstream(t *testing.T) {
	for _, typ := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, tc := range []struct {
			name, model, path, mappingKey string
			mapping                       map[string]any
		}{
			{"source", "gpt-5.3-codex-spark", "/v1/responses", "model_mapping", map[string]any{"gpt-5.3-codex-spark": "gpt-5.4"}},
			{"target", "legacy", "/v1/responses", "model_mapping", map[string]any{"legacy": "openai/gpt5.3codexspark-high"}},
			{"wildcard", "legacy-client", "/v1/responses", "model_mapping", map[string]any{"legacy*": "gpt-5.3-codex-spark"}},
			{"compact", "gpt-5.4", "/v1/responses/compact", "compact_model_mapping", map[string]any{"gpt-5.4": "gpt-5.3-codex-spark-openai-compact"}},
		} {
			t.Run(typ+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int32
				svc := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(*http.Request, string, int64, int) (*http.Response, error) {
					calls.Add(1)
					return ordinaryModelsUpstreamResponse(`{}`), nil
				}})
				account := &Account{Platform: PlatformOpenAI, Type: typ, Credentials: map[string]any{tc.mappingKey: tc.mapping}}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, nil)
				body, err := json.Marshal(map[string]any{"model": tc.model, "input": []any{}})
				require.NoError(t, err)
				_, err = svc.Forward(context.Background(), c, account, body)
				require.ErrorContains(t, err, "retired")
				require.Equal(t, http.StatusNotFound, c.Writer.Status())
				require.Zero(t, calls.Load())
			})
		}
	}
}

func TestSparkRetirementCatalogCompletionCannotRestoreSource(t *testing.T) {
	account := newCodexModelsAPIKeyTestAccount("https://custom.example/v1")
	svc := &OpenAIGatewayService{}
	body := []byte(`{"models":[{"slug":"gpt-5.3-codex-spark"},{"slug":"custom-model","custom":true}]}`)
	manifest := &OpenAIModelsResponse{Body: []byte(`{"models":[]}`), upstreamSourceBody: body}
	require.NoError(t, svc.CompleteAPIKeyCodexModelsManifestForClient(manifest, account))
	require.False(t, containsManifestModel(manifest.Body, "gpt-5.3-codex-spark"))
	require.True(t, containsManifestModel(manifest.Body, "custom-model"))
	require.Equal(t, codexModelsManifestBodyETag(manifest.Body), manifest.ETag)

	groupID := int64(1)
	account.Credentials["model_mapping"] = map[string]any{"gpt-5.3-codex-spark": "custom-model", "legacy": "gpt-5.3-codex-spark", "custom-model": "custom-model"}
	gateway := &GatewayService{accountRepo: codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{groupID: {*account}}}}
	require.Equal(t, []string{"custom-model"}, gateway.GetAvailableModels(context.Background(), &groupID, PlatformOpenAI))
	gateway.modelsListCache = gocache.New(time.Minute, 0)
	gateway.modelsListCache.Set(modelsListCacheKey(&groupID, PlatformOpenAI), []string{"gpt5.3codexspark", "muse-spark"}, time.Minute)
	require.Equal(t, []string{"muse-spark"}, gateway.GetAvailableModels(context.Background(), &groupID, PlatformOpenAI))
}

func TestSparkRetirementCatalogProjectionAndAdminFallback(t *testing.T) {
	for _, typ := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, codex := range []bool{false, true} {
			field, idField := "data", "id"
			if codex {
				field, idField = "models", "slug"
			}
			account := &Account{Platform: PlatformOpenAI, Type: typ, Credentials: map[string]any{"model_mapping": map[string]any{
				"gpt-5.3-codex-spark": "custom-live", "legacy": "gpt-5.3-codex-spark", "custom*": "custom-live",
			}}}
			group := &Group{Platform: PlatformOpenAI, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"legacy", "gpt-5.3-codex-spark", "custom-client"}}}
			body, err := json.Marshal(map[string]any{field: []map[string]any{{idField: "gpt-5.3-codex-spark"}, {idField: "custom-live", "custom": true}}, "unknown_top": true})
			require.NoError(t, err)
			projected, err := projectAccountModelsBody(body, account, group, codex)
			require.NoError(t, err)
			require.NotContains(t, string(projected), "spark")
			require.NotContains(t, string(projected), "legacy")
			require.Contains(t, string(projected), "custom-client")
			require.True(t, gjson.GetBytes(projected, "unknown_top").Bool())
			for _, model := range MergeOpenAIAccountTestModels([]openai.Model{{ID: "gpt-5.3-codex-spark"}}, account) {
				require.NotEqual(t, "legacy", model.ID)
				require.False(t, openai.IsRetiredSparkModel(model.ID))
			}
			account.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-5.3-codex-spark": "gpt-5.3-codex-spark"}}
			require.Empty(t, openAIConfiguredCodexModelIDsForGroup([]Account{*account}, nil), "retired-only catalog must not fall back to defaults")
		}
	}
}

func TestSparkRetirementLiveAndCachedCatalogETag(t *testing.T) {
	for _, standard := range []bool{false, true} {
		for _, age := range []time.Duration{0, 2 * time.Minute} {
			field, idField := "models", "slug"
			if standard {
				field, idField = "data", "id"
			}
			body, err := json.Marshal(map[string]any{field: []map[string]any{{idField: "openai/gpt5.3codexspark-high"}, {idField: "muse-spark", "custom": true}}, "unknown_top": true})
			require.NoError(t, err)
			svc := &OpenAIGatewayService{}
			request := openAIModelsRequest{url: "https://upstream.example/models", standardModelsList: standard}
			oldETag := codexModelsManifestBodyETag(body)
			manifest := &OpenAIModelsResponse{Body: body, ETag: oldETag, upstreamETag: oldETag}
			fetch := func(context.Context, string) (*OpenAIModelsResponse, error) { return manifest, nil }
			// Live cold fetch and an already-populated cache must apply the same filter.
			live, err := svc.fetchCachedOpenAIModels(context.Background(), request, fetch, oldETag)
			require.NoError(t, err)
			svc.openAIModelsCache.set(buildOpenAIModelsCacheKey(request), manifest, time.Now().Add(-age))
			got, err := svc.fetchCachedOpenAIModels(context.Background(), request, fetch, oldETag)
			require.NoError(t, err)
			require.False(t, got.NotModified)
			require.Equal(t, live.ETag, got.ETag)
			require.NotEqual(t, oldETag, got.ETag)
			require.Equal(t, codexModelsManifestBodyETag(got.Body), got.ETag)
			require.Len(t, gjson.GetBytes(got.Body, field).Array(), 1)
			require.Equal(t, "muse-spark", gjson.GetBytes(got.Body, field+".0."+idField).String())
			require.True(t, gjson.GetBytes(got.Body, field+".0.custom").Bool())
			require.True(t, gjson.GetBytes(got.Body, "unknown_top").Bool())
			require.Equal(t, body, manifest.Body, "shared source cache remains intact")
			notModified, err := svc.fetchCachedOpenAIModels(context.Background(), request, fetch, got.ETag)
			require.NoError(t, err)
			require.True(t, notModified.NotModified)
		}
	}
}

func TestSparkRetirementUpstreamDiscoveryAndSnapshot(t *testing.T) {
	for _, typ := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		body := `{"data":[{"id":"gpt-5.3-codex-spark"},{"id":"muse-spark","reasoning":false,"input_modalities":["text"],"context_window":1000}]}`
		upstream := &codexModelsHTTPUpstreamStub{do: func(*http.Request, string, int64, int) (*http.Response, error) {
			return ordinaryModelsUpstreamResponse(body), nil
		}}
		account := &Account{Platform: PlatformOpenAI, Type: typ, Credentials: map[string]any{"access_token": "test", "api_key": "test", "model_mapping": map[string]any{"legacy": "gpt5.3codexspark"}}}
		svc := &AccountTestService{httpUpstream: upstream, cfg: &config.Config{}}
		catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
		require.NoError(t, err)
		require.Equal(t, []string{"muse-spark"}, catalog.Models)
		require.Len(t, catalog.Metadata, 1)
		require.Empty(t, configuredUpstreamModelsForCapabilitySync(account))
		account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Models: map[string]UpstreamModelMetadata{"gpt-5.3-codex-spark": {ID: "gpt-5.3-codex-spark"}, "muse-spark": {ID: "muse-spark"}}})
		require.Len(t, account.GetUpstreamModelMetadataSnapshot().Models, 1)
		body = `{"data":[{"id":"gpt-5.3-codex-spark"}]}`
		catalog, err = svc.SyncUpstreamModelCatalog(context.Background(), account)
		require.NoError(t, err)
		require.Empty(t, catalog.Models)
	}
}

func TestGPT55RetirementModelSupport(t *testing.T) {
	for _, typ := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey} {
		for _, passthrough := range []bool{false, true} {
			t.Run(typ+"/passthrough="+map[bool]string{true: "true", false: "false"}[passthrough], func(t *testing.T) {
				account := &Account{Platform: PlatformOpenAI, Type: typ, Extra: map[string]any{"openai_passthrough": passthrough}}
				for _, model := range []string{"gpt-5.5", "openai/gpt-5.5-high", "gpt-5.5-2026-04-23", "GPT_5.5", "gpt-5.5-openai-compact"} {
					require.Equal(t, typ == AccountTypeAPIKey, account.IsModelSupported(model), model)
				}
				require.True(t, account.IsModelSupported("gpt-6.1-sol"))
			})
		}
	}
	for _, mapping := range []map[string]any{
		{"legacy": "gpt-5.5"}, {"legacy*": "gpt-5.5"},
	} {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": mapping}}
		require.False(t, account.IsModelSupported("legacy"))
	}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.5": "gpt-6.1-sol"}}}
	require.True(t, account.IsModelSupported("gpt-5.5"), "explicit administrator replacement remains authoritative")
	account.Type = AccountTypeAPIKey
	require.True(t, account.IsModelSupported("gpt-5.5"))
	require.False(t, isRetiredOpenAISubscriptionModel(&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, "gpt-5.5"))
	require.False(t, isRetiredOpenAISubscriptionModel(account, "gpt-5.50"))
}

func TestGPT55RetirementCatalogs(t *testing.T) {
	oauth := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	api := Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	group := &Group{ID: 1, Platform: PlatformOpenAI}
	for _, tc := range []struct {
		name      string
		accounts  []Account
		wantGPT55 bool
	}{
		{"oauth", []Account{oauth}, false}, {"api", []Account{api}, true}, {"mixed", []Account{oauth, api}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{1: tc.accounts}}
			gateway := &GatewayService{accountRepo: repo}
			models := gateway.GetAvailableModels(context.Background(), &group.ID, PlatformOpenAI)
			if models != nil {
				require.Equal(t, tc.wantGPT55, containsModelID(models, "gpt-5.5"))
			}
			body, err := buildCodexModelsManifestForAccounts(PlatformOpenAI, []string{"gpt-5.5", "gpt-6.1-sol"}, tc.accounts, group, nil, true)
			require.NoError(t, err)
			require.Equal(t, tc.wantGPT55, containsManifestModel(body, "gpt-5.5"))
			// A stale/pinned upstream body must be filtered before evaluating its old ETag.
			original := []byte(`{"models":[{"slug":"gpt-5.5","unknown":1},{"slug":"gpt-6.1-sol","unknown":2}],"unknown_top":3}`)
			etag := CodexModelsManifestETag(original)
			manifest := &OpenAIModelsResponse{Body: original, ETag: etag}
			service := &OpenAIGatewayService{accountRepo: repo}
			require.NoError(t, service.MergeGroupConfiguredCodexModels(context.Background(), group, manifest, etag))
			if !tc.wantGPT55 {
				require.False(t, manifest.NotModified)
				require.NotEqual(t, etag, manifest.ETag)
				require.False(t, containsManifestModel(manifest.Body, "gpt-5.5"))
				require.Equal(t, int64(3), gjson.GetBytes(manifest.Body, "unknown_top").Int())
				require.Equal(t, int64(2), gjson.GetBytes(manifest.Body, "models.0.unknown").Int())
			}
		})
	}
	// All configured entries retired: plain discovery must remain an authoritative empty list.
	oauth.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-5.5": "gpt-5.5"}}
	repo := codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{1: {oauth}}}
	models := (&GatewayService{accountRepo: repo}).GetAvailableModels(context.Background(), &group.ID, PlatformOpenAI)
	require.NotNil(t, models)
	require.Empty(t, models)
}

func containsModelID(models []string, id string) bool {
	for _, model := range models {
		if model == id {
			return true
		}
	}
	return false
}
func containsManifestModel(body []byte, id string) bool {
	for _, m := range gjson.GetBytes(body, "models").Array() {
		if m.Get("slug").String() == id {
			return true
		}
	}
	return false
}

func TestGPT55RetirementAccountProjection(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"gpt-5.5"},{"id":"gpt-6.1-sol","unknown":true}],"unknown_top":true}`)
	for _, typ := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		account := &Account{Platform: PlatformOpenAI, Type: typ}
		got, err := projectAccountModelsBody(body, account, nil, false)
		require.NoError(t, err)
		require.Equal(t, typ == AccountTypeAPIKey, strings.Contains(string(got), "gpt-5.5"))
		require.True(t, gjson.GetBytes(got, "unknown_top").Bool())
		require.True(t, json.Valid(got))
	}
}

func TestGPT55RetirementProjectionUsesPublicNames(t *testing.T) {
	for _, codex := range []bool{false, true} {
		field, idField := "data", "id"
		if codex {
			field, idField = "models", "slug"
		}
		for _, tc := range []struct {
			name        string
			mapping     map[string]any
			passthrough bool
			api         bool
			want        map[string]string
		}{
			{"one-hop", map[string]any{"legacy": "gpt-5.5", "gpt-5.5": "gpt-6.1-sol"}, false, false, map[string]string{"gpt-5.5": "live"}},
			{"crossed", map[string]any{"gpt-6.1-sol": "gpt-5.5", "gpt-5.5": "gpt-6.1-sol"}, false, false, map[string]string{"gpt-5.5": "live"}},
			{"wildcard", map[string]any{"legacy*": "gpt-5.5", "gpt-5.5": "gpt-6.1-sol"}, false, false, map[string]string{"gpt-5.5": "live"}},
			{"passthrough", map[string]any{"gpt-6.1-sol": "gpt-5.5", "gpt-5.5": "gpt-6.1-sol"}, true, false, map[string]string{"gpt-6.1-sol": "live"}},
			{"api", map[string]any{"legacy": "gpt-5.5", "gpt-5.5": "gpt-6.1-sol"}, false, true, map[string]string{"legacy": "retired", "gpt-5.5": "live"}},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Credentials: map[string]any{"model_mapping": tc.mapping}, Extra: map[string]any{"openai_passthrough": tc.passthrough}}
				if tc.api {
					account.Type = AccountTypeAPIKey
				}
				group := &Group{Platform: PlatformOpenAI, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"legacy", "gpt-5.5", "gpt-6.1-sol"}}}
				body, err := json.Marshal(map[string]any{field: []map[string]any{
					{idField: "gpt-5.5", "source": "retired"}, {idField: "gpt-6.1-sol", "source": "live"},
				}, "unknown_top": true})
				require.NoError(t, err)
				original := string(body)
				got, err := projectAccountModelsBody(body, account, group, codex)
				require.NoError(t, err)
				require.Equal(t, original, string(body), "shared upstream cache must remain unchanged")
				require.True(t, gjson.GetBytes(got, "unknown_top").Bool())
				entries := gjson.GetBytes(got, field).Array()
				require.Len(t, entries, len(tc.want), string(got))
				for _, entry := range entries {
					id := entry.Get(idField).String()
					require.Contains(t, tc.want, id)
					require.Equal(t, tc.want[id], entry.Get("source").String(), "metadata must come from the one-hop upstream target")
					require.True(t, account.IsModelSupported(id), "catalog and routing must agree for %s", id)
				}
				if codex {
					response := &OpenAIModelsResponse{Body: body, ETag: CodexModelsManifestETag(body)}
					require.NoError(t, ApplyPinnedCodexModelsMapping(response, account, group))
					require.JSONEq(t, string(got), string(response.Body))
					require.Equal(t, codexModelsManifestBodyETag(response.Body), response.ETag)
					group.ID = 1
					group.CodexModelsManifestConfig.Enabled = true
					service := &OpenAIGatewayService{accountRepo: codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{1: {*account}}}}
					require.NoError(t, service.MergeGroupConfiguredCodexModels(context.Background(), group, response, CodexModelsManifestETag(body)))
					require.False(t, response.NotModified, "an upstream ETag must not hide the projected catalog")
					require.Equal(t, codexModelsManifestBodyETag(response.Body), response.ETag)
					require.NoError(t, service.MergeGroupConfiguredCodexModels(context.Background(), group, response, response.ETag))
					require.True(t, response.NotModified, "the final public ETag must support conditional requests")
				}
			})
		}
	}
}

func TestGPT55RetirementCompactFallbackKeepsSelectedModel(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
			Credentials: map[string]any{"access_token": "test-token"}, Extra: map[string]any{"openai_passthrough": passthrough}}
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"compact","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))}}
		svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAICompactModel: "gpt-5.5"}}, httpUpstream: upstream}
		require.Empty(t, svc.resolveOpenAICompactFallbackModel(account, "gpt-6.1-sol"))
		c := newOpenAICompactFallbackTestContext(t, "/v1/responses/compact")
		_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6.1-sol","input":[],"instructions":"compact","stream":false}`))
		require.NoError(t, err)
		require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(upstream.lastBody, "model").String())
		account.Type = AccountTypeAPIKey
		require.Equal(t, "gpt-5.5", svc.resolveOpenAICompactFallbackModel(account, "gpt-6.1-sol"))
	}
}

func TestGPT55RetirementSchedulerAndDiagnosis(t *testing.T) {
	ctx := context.Background()
	groupID := int64(1)
	oauth := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	api := Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
	for _, typ := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey} {
		account := oauth
		account.Type = typ
		scheduler := &defaultOpenAIAccountScheduler{}
		ok, _ := scheduler.isAccountRequestCompatibleReason(ctx, &account, OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.5"})
		require.Equal(t, typ == AccountTypeAPIKey, ok)
		reason := openAICompatibleAccountEligibilityFailureReasonBeforeProfit(ctx, &account, PlatformOpenAI, "gpt-5.5", false, "")
		require.Equal(t, typ == AccountTypeAPIKey, reason == "")
	}
	oauth.Credentials = map[string]any{"compact_model_mapping": map[string]any{"gpt-6.1-sol": "gpt-5.5"}}
	scheduler := &defaultOpenAIAccountScheduler{}
	ok, reason := scheduler.isAccountRequestCompatibleReason(ctx, &oauth, OpenAIAccountScheduleRequest{RequestedModel: "gpt-6.1-sol", RequireCompact: true})
	require.False(t, ok)
	require.Equal(t, "model_not_supported", reason)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAICompactModel: "gpt-6.1-sol"}}}
	require.Empty(t, svc.resolveOpenAICompactFallbackModel(&oauth, "gpt-6.1-sol"), "native compact retries must not use a retired explicit target")
	oauth.Credentials = map[string]any{"compact_model_mapping": map[string]any{"gpt-5.5": "gpt-6.1-sol"}}
	ok, _ = scheduler.isAccountRequestCompatibleReason(ctx, &oauth, OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.5", RequireCompact: true})
	require.True(t, ok, "an explicit compact replacement remains usable")
	for _, accounts := range [][]Account{{oauth}, {oauth, api}} {
		svc := &OpenAIGatewayService{accountRepo: codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{groupID: accounts}}}
		diagnosis := svc.DiagnoseModelAvailabilityForPlatform(ctx, &groupID, "gpt-5.5", PlatformOpenAI)
		require.True(t, diagnosis.HasAccountsInPool)
		require.True(t, diagnosis.HasRetiredSubscriptionModel)
		require.Equal(t, len(accounts) == 2, diagnosis.HasModelSupport)
	}
}

func TestGPT55RetirementEmptyListingSource(t *testing.T) {
	// A configured alias targeting retired GPT-5.5 must not reappear in a generated catalog.
	account := Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"legacy": "gpt-5.5"}}}
	require.Empty(t, openAIConfiguredCodexModelIDsForGroup([]Account{account}, nil))
	body, err := buildCodexModelsManifestForAccounts(PlatformComposite, []string{"gpt-5.5", "gpt-6.1-sol"}, []Account{account}, nil, nil, true)
	require.NoError(t, err)
	require.False(t, containsManifestModel(body, "gpt-5.5"))
}
