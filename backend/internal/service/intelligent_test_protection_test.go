package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestApplyIntelligentPayloadPromptReplacesConnectivityPing(t *testing.T) {
	ctx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "请只输出一个独立、有效的 SVG，绘制一只骑自行车的鹈鹕。"})
	payload := createOpenAITestPayload("gpt-5.3-codex", true)
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"text":"hi"`)

	applyIntelligentPayloadPrompt(ctx, payload)
	raw, err = json.Marshal(payload)
	require.NoError(t, err)
	require.Contains(t, string(raw), "骑自行车的鹈鹕")
	require.NotContains(t, string(raw), `"text":"hi"`)
	require.Contains(t, string(raw), `"reasoning":{"effort":"low"}`)
}

func TestApplyIntelligentTestProtectionSetsSessionHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID:       11,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			AntiDegradationExtraKey:      true,
			ProtectionScopeExtraKey:      "legacy",
			AntiDegradeMarkerExtraKey:    map[string]any{"enabled": true, "mode": "legacy"},
			codexFingerprintModeExtraKey: string(codexFingerprintSession),
			codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
		},
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	require.NoError(t, prepareIntelligentTestProtection(c, account, map[string]any{}))
	headers := http.Header{}
	require.NoError(t, applyIntelligentTestProtection(c, account, headers, []byte(`{"model":"gpt-5.3-codex"}`)))
	require.NotEmpty(t, headers.Get("conversation_id"))
	require.NotEmpty(t, headers.Get("thread-id"))
}

func TestIntelligentTestProtectionDoesNotMutateExtra(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &Account{
		ID:       11,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			AntiDegradationExtraKey:      true,
			ProtectionScopeExtraKey:      "legacy",
			AntiDegradeMarkerExtraKey:    map[string]any{"enabled": true, "mode": "legacy"},
			codexFingerprintModeExtraKey: string(codexFingerprintSession),
			codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
			"auto_pause_5h_threshold":    0.95,
		},
	}
	before := cloneExtraJSON(account.Extra)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	require.NoError(t, prepareIntelligentTestProtection(c, account, map[string]any{}))
	require.Equal(t, before, account.Extra)
	require.Equal(t, 0.95, account.Extra["auto_pause_5h_threshold"])
}
