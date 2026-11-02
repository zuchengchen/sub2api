//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountTrafficProtectionRemovedSavePreservesIndependentConfiguration(t *testing.T) {
	for _, old := range []any{nil, false, true, "malformed", map[string]any{"enabled": true, "mode": "mode1", "policy_version": float64(3)}} {
		t.Run(fmt.Sprint(old), func(t *testing.T) {
			a := &Account{ID: 7, Name: "before", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 50, Extra: map[string]any{
				"anti_degradation": old, "anti_degrade": old, "protection_scope": "codex_v3", "account_traffic_control": old,
				codexFingerprintModeExtraKey: "full", codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
				featureKeyCodexImageGenerationBridge: true, featureKeyCodexImageGenerationExplicitToolPolicy: "allow",
				"enable_tls_fingerprint": true, "tls_fingerprint_builtin": "nodejs24", "proxy_mode": "fixed", "auto_pause_5h_threshold": 0.95,
			}}
			repo := &longContextBillingRepoStub{account: a}
			extra := cloneExtraJSON(a.Extra)
			extra["proxy_mode"] = "random"
			svc := &adminServiceImpl{accountRepo: repo}
			updated, err := svc.UpdateAccount(context.Background(), a.ID, &UpdateAccountInput{Name: "saved", Extra: extra})
			require.NoError(t, err)
			require.Equal(t, "saved", updated.Name)
			require.Equal(t, 50, updated.Concurrency)
			require.Equal(t, codexFingerprintFull, updated.GetCodexFingerprintMode())
			require.Equal(t, "11111111-1111-4111-8111-111111111111", updated.Extra[codexFingerprintSeedExtraKey])
			require.Equal(t, boolOverridePtr(true), updated.CodexImageGenerationBridgeOverride())
			require.True(t, updated.IsTLSFingerprintEnabled())
			require.Equal(t, "random", updated.Extra["proxy_mode"])
			require.Equal(t, 0.95, updated.Extra["auto_pause_5h_threshold"])
			require.Equal(t, extra["account_traffic_control"], updated.Extra["account_traffic_control"])
			// Explicit edits to previously locked fields must now take effect.
			extra = cloneExtraJSON(updated.Extra)
			extra[codexFingerprintModeExtraKey] = "device"
			extra["enable_tls_fingerprint"] = false
			updated, err = svc.UpdateAccount(context.Background(), a.ID, &UpdateAccountInput{Extra: extra})
			require.NoError(t, err)
			require.Equal(t, codexFingerprintDevice, updated.GetCodexFingerprintMode())
			require.False(t, updated.IsTLSFingerprintEnabled())
		})
	}
}

func TestAccountTrafficProtectionRemovedIdentityAndIntegrityAreIndependent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, legacy := range []any{nil, false, true, "invalid", map[string]any{"enabled": true, "mode": "mode1", "policy_version": float64(3)}} {
		for _, mode := range []string{"off", "full"} {
			t.Run(fmt.Sprint(legacy)+"/"+mode, func(t *testing.T) {
				a := &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"anti_degradation": legacy, "anti_degrade": legacy, codexFingerprintModeExtraKey: mode, codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111", requestIntegrityModeKey: "enforce", "enable_tls_fingerprint": true}}
				before := cloneExtraJSON(a.Extra)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
				require.NoError(t, prepareIntelligentTestIdentity(c, a, map[string]any{}))
				headers := http.Header{}
				require.NoError(t, applyIntelligentTestIdentity(c, a, headers, []byte(`{"instructions":"valid probe","input":[]}`)))
				require.Equal(t, mode == "full", headers.Get("conversation_id") != "")
				require.Equal(t, mode == "full", headers.Get("thread-id") != "")
				require.Error(t, applyIntelligentTestIdentity(c, a, headers, []byte(`invalid json`)))
				require.Error(t, checkAccountRequestIntegrity(c, a, []byte(`{"instructions":"before"}`), []byte(`{"instructions":"after"}`)))
				require.True(t, a.IsTLSFingerprintEnabled())
				require.Equal(t, before, a.Extra)
			})
		}
	}
}

func TestAccountTrafficProtectionRemovedBulkUpdate(t *testing.T) {
	for _, old := range []any{nil, false, true, "bad", map[string]any{"enabled": true, "mode": "mode1"}} {
		repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 50, Extra: map[string]any{"anti_degradation": old, "anti_degrade": old}}}}
		concurrency := 50
		result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{7}, Concurrency: &concurrency, Extra: map[string]any{"account_traffic_control": old, codexFingerprintModeExtraKey: "full", featureKeyCodexImageGenerationBridge: true, "enable_tls_fingerprint": false, "proxy_mode": "random"}})
		require.NoError(t, err)
		require.Equal(t, 1, result.Success)
		require.Equal(t, 1, repo.bulkUpdateCalls)
		require.Equal(t, "full", repo.lastBulkUpdate.Extra[codexFingerprintModeExtraKey])
		require.Equal(t, true, repo.lastBulkUpdate.Extra[featureKeyCodexImageGenerationBridge])
		require.Equal(t, "random", repo.lastBulkUpdate.Extra["proxy_mode"])
	}
}

func TestAccountTrafficProtectionRemovedProbesReachUpstream(t *testing.T) {
	for _, policy := range []any{"malformed", map[string]any{"strict_rpm_enabled": true, "rpm": 1, "burst": 1, "adaptive_enabled": true, "adaptive_mode": "automatic", "min_concurrency": 1}} {
		a := newCodexProbeTestAccount()
		a.Concurrency = 50
		a.Extra["account_traffic_control"] = policy
		a.Extra["anti_degradation"] = true
		a.Extra["anti_degrade"] = map[string]any{"mode": "mode1", "enabled": true}
		upstream := &codexProbeUpstream{httpUpstreamRecorder: httpUpstreamRecorder{responses: []*http.Response{cookieWSHTTPResponse("True"), cookieWSHTTPResponse("True")}}}
		svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), httpUpstream: upstream}
		for i := 0; i < 2; i++ {
			require.Equal(t, openAITiboHealthy, svc.probeOpenAITiboHTTP(context.Background(), a).verdict)
			requireCodexProbeWire(t, upstream)
		}
		require.Equal(t, 2, upstream.plain+len(upstream.profiles), "both requests must reach the upstream despite old RPM=1")
	}
}
