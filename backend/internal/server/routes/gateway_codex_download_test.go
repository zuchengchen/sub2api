package routes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type codexDownloadRoutesUpstream struct {
	service.HTTPUpstream
	mu       sync.Mutex
	versions []string
}

func (u *codexDownloadRoutesUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	version := req.URL.Query().Get("client_version")
	u.mu.Lock()
	u.versions = append(u.versions, version)
	u.mu.Unlock()
	body := `{"data":[{"id":"gpt-6.2-astra"},{"id":"gpt-6.2-sol"},{"id":"gpt-5.5"}]}`
	if version != "" {
		body = `{"models":[{"slug":"gpt-6.2-astra","description":"current-astra"},{"slug":"gpt-6.2-sol","description":"current-sol"},{"slug":"gpt-5.5"}]}`
		if version == "0.158.0" {
			body = `{"models":[{"slug":"gpt-5.5"}]}`
		}
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestOAuthReliabilityCodexDownloadCanonicalRouteAndPermissions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &pinnedModelsRoutesRepository{account: service.Account{
		ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "synthetic-upstream-key", "base_url": "https://models.example/v1"},
	}}
	upstream := &codexDownloadRoutesUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	svc := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	h := &handler.Handlers{
		Gateway:       handler.NewGatewayHandler(nil, svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil),
		OpenAIGateway: handler.NewOpenAIGatewayHandler(svc, nil, nil, nil, nil, nil, nil, nil, cfg),
		AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
	}
	group := &service.Group{ID: 1, Platform: service.PlatformOpenAI,
		CodexModelsManifestConfig: service.GroupCodexModelsManifestConfig{Enabled: true, AccountIDs: []int64{7}}}
	restricted := *group
	restricted.ID = 2
	restricted.ModelAllowlist = service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6.2-astra"}}
	router := gin.New()
	RegisterGatewayRoutes(router, h, servermiddleware.APIKeyAuthMiddleware(func(c *gin.Context) {
		selected := group
		switch c.GetHeader("Authorization") {
		case "Bearer download-full":
		case "Bearer download-restricted":
			selected = &restricted
		default:
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Set(string(servermiddleware.ContextKeyAPIKey), &service.APIKey{GroupID: &selected.ID, Group: selected})
		c.Next()
	}), nil, nil, nil, nil, nil, cfg)
	request := func(path, key, etag string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", key)
		req.Header.Set("If-None-Match", etag)
		router.ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"/v1/models?format=codex", "/models?format=codex", "/backend-api/codex/models"} {
		t.Run(path, func(t *testing.T) {
			require.Equal(t, http.StatusUnauthorized, request(path, "", "").Code)
			full := request(path, "Bearer download-full", "")
			require.Equal(t, http.StatusOK, full.Code, full.Body.String())
			require.True(t, gjson.GetBytes(full.Body.Bytes(), "models").IsArray(), "download must use Codex manifest dispatch")
			require.False(t, gjson.GetBytes(full.Body.Bytes(), "data").Exists())
			require.Contains(t, full.Body.String(), `"slug":"gpt-6.2-astra"`)
			require.Contains(t, full.Body.String(), `"slug":"gpt-6.2-sol"`)
			filtered := request(path, "Bearer download-restricted", full.Header().Get("ETag"))
			require.Equal(t, http.StatusOK, filtered.Code, "another group's ETag cannot bypass filtering")
			var manifest struct {
				Models []struct {
					Slug string `json:"slug"`
				} `json:"models"`
			}
			require.NoError(t, json.Unmarshal(filtered.Body.Bytes(), &manifest))
			require.Len(t, manifest.Models, 1)
			require.Equal(t, "gpt-6.2-astra", manifest.Models[0].Slug)
			require.NotContains(t, filtered.Body.String(), `"slug":"gpt-6.2-sol"`)
			require.NotEqual(t, full.Header().Get("ETag"), filtered.Header().Get("ETag"))
			require.Equal(t, http.StatusNotModified, request(path, "Bearer download-restricted", filtered.Header().Get("ETag")).Code)
		})
	}
	t.Run("explicit_old_version", func(t *testing.T) {
		for _, path := range []string{"/v1/models?client_version=0.158.0", "/v1/models?format=codex&client_version=0.158.0"} {
			old := request(path, "Bearer download-full", "")
			require.Equal(t, http.StatusOK, old.Code)
			require.Contains(t, old.Body.String(), `"slug":"gpt-5.5"`)
			require.NotContains(t, old.Body.String(), "gpt-6.2-")
		}
	})
	t.Run("ordinary_list_and_single_model", func(t *testing.T) {
		for _, path := range []string{"/v1/models", "/models?client_version=", "/v1/models?format=ordinary"} {
			ordinary := request(path, "Bearer download-full", "")
			require.Equal(t, http.StatusOK, ordinary.Code)
			require.Equal(t, "list", gjson.GetBytes(ordinary.Body.Bytes(), "object").String())
			require.True(t, gjson.GetBytes(ordinary.Body.Bytes(), "data").IsArray())
			require.False(t, gjson.GetBytes(ordinary.Body.Bytes(), "models").Exists())
		}
		model := request("/v1/models/gpt-6.2-astra?format=codex", "Bearer download-full", "")
		require.Equal(t, http.StatusOK, model.Code)
		require.Equal(t, "model", gjson.GetBytes(model.Body.Bytes(), "object").String())
		require.Equal(t, "gpt-6.2-astra", gjson.GetBytes(model.Body.Bytes(), "id").String())
		require.Equal(t, http.StatusNotFound, request("/v1/models/gpt-6.2-sol?format=codex", "Bearer download-restricted", "").Code)
	})
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Contains(t, upstream.versions, service.CodexCanonicalClientVersion(), "the version-free download delegates to the existing server canonical-version resolver")
	require.Contains(t, upstream.versions, "0.158.0", "explicit client negotiation remains unchanged")
}
