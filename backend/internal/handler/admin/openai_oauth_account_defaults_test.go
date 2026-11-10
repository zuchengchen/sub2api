package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthCreateDefaultsAccountEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, batch := range []bool{false, true} {
		for _, tc := range []struct {
			name, platform, accountType string
			provided                    *int
			want                        int
		}{
			{name: "OAuth omitted", platform: service.PlatformOpenAI, accountType: service.AccountTypeOAuth, want: 10},
			{name: "OAuth explicit", platform: service.PlatformOpenAI, accountType: service.AccountTypeOAuth, provided: intPtr(7), want: 7},
			{name: "OAuth unlimited", platform: service.PlatformOpenAI, accountType: service.AccountTypeOAuth, provided: intPtr(0)},
			{name: "API key unchanged", platform: service.PlatformOpenAI, accountType: service.AccountTypeAPIKey},
			{name: "Claude unchanged", platform: service.PlatformAnthropic, accountType: service.AccountTypeOAuth},
		} {
			t.Run(tc.name, func(t *testing.T) {
				stub := newStubAdminService()
				h := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				router := gin.New()
				account := map[string]any{"name": "fixture", "platform": tc.platform, "type": tc.accountType, "credentials": map[string]any{"access_token": "fixture"}}
				if tc.provided != nil {
					account["concurrency"] = *tc.provided
				}
				var payload any = account
				if batch {
					router.POST("/accounts", h.BatchCreate)
					payload = map[string]any{"accounts": []any{account}}
				} else {
					router.POST("/accounts", h.Create)
				}
				body, err := json.Marshal(payload)
				require.NoError(t, err)
				request := httptest.NewRequest(http.MethodPost, "/accounts", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				require.Len(t, stub.createdAccounts, 1)
				require.Equal(t, tc.want, stub.createdAccounts[0].Concurrency)
				require.Equal(t, tc.provided != nil, stub.createdAccounts[0].ConcurrencyProvided)
			})
		}
	}
}

func TestOpenAIOAuthCreateDefaultsCodexImport(t *testing.T) {
	for _, concurrency := range []*int{nil, intPtr(7), intPtr(0)} {
		svc := newCodexImportMemoryAdminService(nil)
		h := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		result, err := h.importCodexSessions(context.Background(), CodexSessionImportRequest{
			Concurrency: concurrency, Extra: map[string]any{"codex_image_generation_bridge": nil}, SkipDefaultGroupBind: boolPtr(true),
		}, []codexImportEntry{{Index: 1, Value: map[string]any{"access_token": "fixture-access-token", "refresh_token": "fixture-refresh"}}})
		require.NoError(t, err)
		require.Equal(t, 1, result.Created)
		require.Len(t, svc.createdAccounts, 1)
		want := 10
		if concurrency != nil {
			want = *concurrency
		}
		require.Equal(t, want, svc.createdAccounts[0].Concurrency)
		require.Equal(t, concurrency != nil, svc.createdAccounts[0].ConcurrencyProvided)
		require.Contains(t, svc.createdAccounts[0].Extra, "codex_image_generation_bridge")
		require.Nil(t, svc.createdAccounts[0].Extra["codex_image_generation_bridge"])
	}
}

func TestOpenAIOAuthCreateDefaultsDataImportPresence(t *testing.T) {
	for _, field := range []string{"", `,"concurrency":0`, `,"concurrency":7`} {
		var req DataImportRequest
		require.NoError(t, json.Unmarshal([]byte(`{"data":{"accounts":[{"name":"fixture","platform":"openai","type":"oauth","credentials":{"access_token":"fixture"}`+field+`}]}}`), &req))
		stub := newStubAdminService()
		h := NewAccountHandler(stub, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		result, err := h.importData(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, 1, result.AccountCreated)
		require.Len(t, stub.createdAccounts, 1)
		require.Equal(t, field != "", stub.createdAccounts[0].ConcurrencyProvided)
		if field == `,"concurrency":7` {
			require.Equal(t, 7, stub.createdAccounts[0].Concurrency)
		} else {
			require.Zero(t, stub.createdAccounts[0].Concurrency)
		}
	}
}

func TestOpenAIOAuthCreateDefaultsImportReplacesExplicitImageStrategy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		extra  map[string]any
		bridge *bool
		policy string
	}{
		{name: "hosted", extra: map[string]any{"codex_image_generation_bridge": true}, bridge: boolPtr(true), policy: "allow"},
		{name: "inherit", extra: map[string]any{"codex_image_generation_bridge": nil}, policy: "allow"},
		{name: "disabled", extra: map[string]any{"codex_image_generation_bridge": false}, bridge: boolPtr(false), policy: "allow"},
		{name: "strip", extra: map[string]any{"codex_image_generation_explicit_tool_policy": "strip"}, policy: "strip"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Credentials: map[string]any{"access_token": "fixture-token", "refresh_token": "fixture-refresh"},
				Extra: map[string]any{"codex_image_generation_bridge_enabled": false, "codex_image_generation_explicit_tool_policy": "strip",
					"openai": map[string]any{"codex_image_generation_bridge": false, "codex_image_generation_explicit_tool_policy": "strip", "unrelated": "keep"}, "custom": "keep"}}
			svc := newCodexImportMemoryAdminService([]service.Account{existing})
			h := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			result, err := h.importCodexSessions(context.Background(), CodexSessionImportRequest{Extra: tc.extra}, []codexImportEntry{{Index: 1, Value: map[string]any{"access_token": "fixture-token", "refresh_token": "fixture-refresh"}}})
			require.NoError(t, err)
			require.Equal(t, 1, result.Updated)
			require.Len(t, svc.updatedAccounts, 1)
			actual := &service.Account{Platform: service.PlatformOpenAI, Extra: svc.updatedAccounts[0].input.Extra}
			require.Equal(t, tc.bridge, actual.CodexImageGenerationBridgeOverride())
			require.Equal(t, tc.policy, actual.CodexImageGenerationExplicitToolPolicy())
			require.Equal(t, "keep", actual.Extra["custom"])
			require.Equal(t, "keep", actual.Extra["openai"].(map[string]any)["unrelated"])
			require.Equal(t, "strip", existing.Extra["codex_image_generation_explicit_tool_policy"])
		})
	}
}

func TestOpenAIOAuthCreateDefaultsImportKeepsBridgeForToolPolicyUpdate(t *testing.T) {
	for _, policy := range []any{"allow", nil} {
		existing := map[string]any{
			"codex_image_generation_bridge":               true,
			"codex_image_generation_bridge_enabled":       false,
			"codex_image_generation_explicit_tool_policy": "strip",
			"openai": map[string]any{"codex_image_generation_bridge": false, "custom": "keep"},
			"custom": "keep",
		}
		merged := mergeCodexImportExtra(existing, map[string]any{"codex_image_generation_explicit_tool_policy": policy})
		account := &service.Account{Platform: service.PlatformOpenAI, Extra: merged}
		require.Equal(t, boolPtr(true), account.CodexImageGenerationBridgeOverride())
		require.Equal(t, "allow", account.CodexImageGenerationExplicitToolPolicy())
		require.Equal(t, false, merged["codex_image_generation_bridge_enabled"])
		require.Equal(t, existing["openai"], merged["openai"])
		require.Equal(t, "keep", merged["custom"])
		require.Equal(t, "strip", existing["codex_image_generation_explicit_tool_policy"])
	}
}
