//go:build unit

package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTiboHealthyGroupModelCanonicalLunaAliasesHTTPForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, group, requested, upstream string
		status                           int
	}{
		// Unknown names retain their identity; the healthy group's historical
		// Luna exclusion is a local admission policy, not a global alias rule.
		{"welfare_lunatic", service.TiboWelfareGroupName, "gpt-5.6-lunatic", "gpt-5.6-lunatic", http.StatusOK},
		{"welfare_lunaevil", service.TiboWelfareGroupName, "gpt-5.6-lunaevil", "gpt-5.6-lunaevil", http.StatusOK},
		{"welfare_luna_alias", service.TiboWelfareGroupName, "openai/GPT5.6LUNA-max", "gpt-5.6-luna", http.StatusOK},
		{"healthy_lunatic", service.TiboHealthyGroupName, "gpt-5.6-lunatic", "gpt-5.6-luna", http.StatusNotFound},
		{"healthy_lunaevil", service.TiboHealthyGroupName, "gpt-5.6-lunaevil", "gpt-5.6-luna", http.StatusNotFound},
		{"healthy_prefixed_lunatic", service.TiboHealthyGroupName, "provider/GPT5.6LUNATIC", "", http.StatusNotFound},
		{"healthy_luna_alias", service.TiboHealthyGroupName, "openai/GPT5.6LUNA-max", "", http.StatusNotFound},
		{"healthy_unrelated_lunatic", service.TiboHealthyGroupName, "gpt-6-lunatic", "gpt-6-lunatic", http.StatusOK},
		{"healthy_private_sol", service.TiboHealthyGroupName, "gpt-5.6-sol-private", "gpt-5.6-sol-private", http.StatusOK},
		{"healthy_future_family", service.TiboHealthyGroupName, "gpt-5.7", "gpt-5.7", http.StatusOK},
		{"healthy_known_sol_alias", service.TiboHealthyGroupName, "gpt-5.6sol", "gpt-5.6-sol", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newAstraProCapturedUpstream(&http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"id":"resp_tibo_alias","model":"` + tc.upstream + `","status":"completed",` +
					`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],` +
					`"usage":{"input_tokens":1,"output_tokens":2}}`)),
			})
			handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
			groupID := int64(3131)
			group := &service.Group{ID: groupID, Name: tc.group, Platform: service.PlatformOpenAI, Status: service.StatusActive}
			router := gin.New()
			router.Use(func(c *gin.Context) {
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
					ID: 99, GroupID: &groupID, Group: group, User: &service.User{ID: 100},
				})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 100, Concurrency: 0})
				c.Next()
			})
			router.Use(middleware.GroupModelAllowlist())
			router.POST("/v1/responses", handler.Responses)
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+tc.requested+`","stream":false,"input":"hello"}`))
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)

			urls, accountIDs, bodies := upstream.snapshot()
			require.Equal(t, tc.status, recorder.Code, recorder.Body.String())
			if tc.status == http.StatusNotFound {
				require.Equal(t, "model_not_found", gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
				require.Empty(t, accountIDs, "deny before the OAuth upstream transport")
				return
			}
			require.Len(t, accountIDs, 1, "positive controls must really reach the OAuth upstream")
			require.Equal(t, []string{astraProCodexURL}, urls)
			require.Len(t, bodies, 1)
			require.Equal(t, tc.upstream, gjson.GetBytes(bodies[0], "model").String(),
				"admission must account for the model identity actually sent by the forwarding layer")
		})
	}
}
