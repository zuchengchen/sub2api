package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

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
