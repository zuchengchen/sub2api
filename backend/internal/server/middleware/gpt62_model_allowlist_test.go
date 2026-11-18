package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGPT62IngressRequiresExplicitGroupAccess(t *testing.T) {
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
			t.Run(model+path, func(t *testing.T) {
				key := allowlistAPIKey(true, "gpt-6-astra", "gpt-6.1-sol")
				key.Group.Platform = service.PlatformOpenAI
				router, calls := newGroupModelAllowlistTestRouter(key, "/v1")
				body := `{"model":"` + model + `","input":"probe"}`
				denied := doJSON(t, router, http.MethodPost, path, body)
				require.Equal(t, http.StatusNotFound, denied.Code)
				require.Empty(t, *calls, "adding model support must not bypass administrator restrictions")
				key.Group.ModelAllowlist.Models = append(key.Group.ModelAllowlist.Models, model)
				allowed := doJSON(t, router, http.MethodPost, path, body)
				require.Equal(t, http.StatusOK, allowed.Code, allowed.Body.String())
				require.Len(t, *calls, 1)
			})
		}
	}
}

func TestGPT62IngressAliasesPreserveRequestIdentity(t *testing.T) {
	for _, model := range []string{"gpt-6.2-astra", "gpt-6.2-sol"} {
		for _, requested := range []string{model, model + "-high", "openai/" + model + "-max"} {
			t.Run(requested, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				router := gin.New()
				key := allowlistAPIKey(true, model)
				key.Group.Platform = service.PlatformOpenAI
				router.Use(func(c *gin.Context) { c.Set(string(ContextKeyAPIKey), key) })
				router.Use(GroupModelAllowlist())
				body := `{"model":"` + requested + `","input":"probe"}`
				router.POST("/v1/responses", func(c *gin.Context) {
					raw, err := io.ReadAll(c.Request.Body)
					require.NoError(t, err)
					require.Equal(t, body, string(raw), "admission must not change the requested model")
					c.Status(http.StatusNoContent)
				})
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, req)
				require.Equal(t, http.StatusNoContent, response.Code, response.Body.String())
			})
		}
	}
}
