package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityContinuationResponsesRejectsPartialAndCrossFamilyContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, input := range []string{
		`{"type":"function_call","call_id":"one","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"two","output":"keep synthetic orphan"}`,
		`{"type":"function_call","call_id":"one","name":"lookup","arguments":"{}"},{"type":"custom_tool_call_output","call_id":"one","output":"wrong family"}`,
		`{"type":"computer_call_output","call_id":"one","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,eA=="}}`,
		`{"type":"shell_call_output","call_id":"one","output":[]}`,
		`{"type":"apply_patch_call_output","call_id":"one","status":"completed"}`,
		`{"type":"tool_search_output","call_id":"one","execution":"client","status":"completed","tools":[]}`,
	} {
		body := `{"model":"gpt-5.1","stream":false,"input":[` + input + `]}`
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		groupID := int64(2)
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 101, UserID: 1, GroupID: &groupID, User: &service.User{ID: 1}})
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1, Concurrency: 1})
		h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
		h.Responses(c)
		require.Equal(t, http.StatusBadRequest, rec.Code, input)
		require.Equal(t, "continuation_context_missing", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
		require.Equal(t, "input", gjson.GetBytes(rec.Body.Bytes(), "error.param").String())
		require.NotContains(t, rec.Body.String(), "synthetic orphan")
	}
}

func TestOAuthReliabilityContinuationResponsesAccountFailoverRequiresPortableHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name     string
		body     string
		portable bool
	}{
		{"native_anchor", `{"previous_response_id":"resp_external","input":[{"role":"user","content":"only a delta"}]}`, false},
		{"item_reference", `{"input":[{"type":"item_reference","id":"call_remote"},{"type":"function_call_output","call_id":"call_remote","output":"preserve result"}]}`, false},
		{"encrypted_owner_state", `{"input":[{"type":"reasoning","summary":[],"encrypted_content":"synthetic-ciphertext"},{"role":"user","content":"continue"}]}`, false},
		{"complete_call_result", `{"input":[{"role":"user","content":"root"},{"type":"function_call","call_id":"call_one","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"call_one","output":"complete result"}]}`, true},
		{"stateless_text", `{"input":"root"}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			groupID := int64(4203)
			accounts := []service.Account{
				{ID: 9910, Name: "first", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 1,
					Credentials: map[string]any{"api_key": "synthetic-first", "base_url": "https://api.example.test"}, Extra: map[string]any{"openai_passthrough": true}},
				{ID: 9911, Name: "second", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Priority: 2,
					Credentials: map[string]any{"api_key": "synthetic-second", "base_url": "https://api.example.test"}, Extra: map[string]any{"openai_passthrough": true}},
			}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			cfg.Gateway.MaxAccountSwitches = 1
			upstream := &openAIHTTPPassthroughFailoverUpstream{}
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			gateway := service.NewOpenAIGatewayService(&openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}, nil, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			gateway.DisableOpenAITiboRouteForTest()
			if tt.name == "native_anchor" {
				// The Responses HTTP entry requires an established tenant owner
				// before its account failover boundary can be exercised.
				require.NoError(t, gateway.BindOpenAIHTTPResponseOwner(context.Background(), groupID, "resp_external", 1703, 1803))
			}
			h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			body := `{"model":"gpt-5.2","stream":false,` + strings.TrimPrefix(tt.body, "{")
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 1803, UserID: 1703, GroupID: &groupID, User: &service.User{ID: 1703, Status: service.StatusActive}, Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1703, Concurrency: 0})
			h.Responses(c)
			if tt.portable {
				require.Equal(t, []int64{9910, 9911}, upstream.calls())
				require.Equal(t, http.StatusBadGateway, rec.Code)
			} else {
				require.Equal(t, []int64{9910}, upstream.calls())
				require.Equal(t, http.StatusBadRequest, rec.Code)
				require.Equal(t, "continuation_context_missing", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
			}
		})
	}
}
