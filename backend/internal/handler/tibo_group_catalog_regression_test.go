package handler

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestTiboHealthyGroupModelPinnedStandardCacheAndDetails(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "pinned"
		if fallback {
			name = "scheduler-fallback"
		}
		t.Run(name, func(t *testing.T) {
			const sourceBody = `{"data":[{"id":"gpt-6-luna"},{"id":"gpt-5.6-luna"},{"id":"luna"},{"id":"openai/GPT5.6LUNA-max"},{"id":"gpt-6-astra"},{"id":"gpt-6-sol","object":"model","created":123,"owned_by":"source-owner","retained_metadata":{"context":424242}},{"id":"gpt-6.1-terra"},{"id":"gpt-image-2"}]}`
			account := newPinnedCodexAccount(8781, service.StatusActive, true, false)
			upstream := &codexModelsPinnedHTTPUpstream{bodies: map[int64]string{account.ID: sourceBody}}
			codex := newPinnedCodexTestHandler([]service.Account{account}, upstream, 3)
			handler := &GatewayHandler{openAIGatewayService: codex.gatewayService, maxAccountSwitches: 3}
			config := service.GroupCodexModelsManifestConfig{Enabled: true, AccountIDs: []int64{account.ID}}
			if fallback {
				config.AccountIDs = []int64{8799}
				config.FallbackToScheduler = true
			}
			other := &service.Group{
				ID: 8782, Name: service.TiboWelfareGroupName, Platform: service.PlatformOpenAI,
				CodexModelsManifestConfig: config,
			}
			healthy := &service.Group{
				ID: 8783, Name: service.TiboHealthyGroupName, Platform: service.PlatformOpenAI,
				ModelAllowlist:            service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-luna"}},
				CodexModelsManifestConfig: config,
			}
			firstOther := requestModelForTest(handler, other, "", "")
			require.Equal(t, http.StatusOK, firstOther.Code, firstOther.Body.String())
			require.ElementsMatch(t, []string{"gpt-6-luna", "gpt-5.6-luna", "luna", "openai/GPT5.6LUNA-max", "gpt-6-astra", "gpt-6-sol", "gpt-6.1-terra", "gpt-image-2"}, ordinaryPinnedModelIDs(t, firstOther))
			oldETag := firstOther.Header().Get("ETag")
			require.NotEmpty(t, oldETag)
			firstHealthy := requestModelForTest(handler, healthy, "", "")
			require.Equal(t, http.StatusOK, firstHealthy.Code, firstHealthy.Body.String())
			require.ElementsMatch(t, []string{"gpt-6-luna", "gpt-5.6-luna", "luna", "openai/GPT5.6LUNA-max", "gpt-6-astra", "gpt-6-sol", "gpt-6.1-terra", "gpt-image-2"}, ordinaryPinnedModelIDs(t, firstHealthy),
				"the old Luna-only allowlist must not exclude other servable models")
			newETag := firstHealthy.Header().Get("ETag")
			require.NotEmpty(t, newETag)
			require.Equal(t, oldETag, newETag)
			require.Equal(t, service.CodexModelsManifestETag(firstHealthy.Body.Bytes()), newETag)
			cached := requestModelForTest(handler, healthy, "", newETag)
			require.Equal(t, http.StatusNotModified, cached.Code)
			require.Empty(t, cached.Body.String())
			require.Equal(t, newETag, cached.Header().Get("ETag"))

			// A collection ETag cannot turn a detail request into 304. The detail
			// must retain the source's complete metadata after group filtering.
			detail := requestModelForTest(handler, healthy, "gpt-6-sol", newETag)
			require.Equal(t, http.StatusOK, detail.Code, detail.Body.String())
			require.Empty(t, detail.Header().Get("ETag"))
			require.JSONEq(t, `{"id":"gpt-6-sol","object":"model","created":123,"owned_by":"source-owner","retained_metadata":{"context":424242}}`, detail.Body.String())
			for _, model := range []string{"gpt-6-luna", "gpt-5.6-luna", "luna", "openai/GPT5.6LUNA-max"} {
				healthyDetail := requestModelForTest(handler, healthy, model, newETag)
				require.Equal(t, http.StatusOK, healthyDetail.Code, model+": "+healthyDetail.Body.String())
				allowed := requestModelForTest(handler, other, model, oldETag)
				require.Equal(t, http.StatusOK, allowed.Code, model+": "+allowed.Body.String())
			}
			for _, model := range []string{"gpt-6-astra", "gpt-6.1-terra", "gpt-image-2"} {
				allowed := requestModelForTest(handler, healthy, model, newETag)
				require.Equal(t, http.StatusOK, allowed.Code, model+": "+allowed.Body.String())
			}
			otherAgain := requestModelForTest(handler, other, "", "")
			require.Equal(t, http.StatusOK, otherAgain.Code, otherAgain.Body.String())
			require.Equal(t, firstOther.Body.String(), otherAgain.Body.String(), "same-account source cache must survive another group's filtering")
			require.Equal(t, oldETag, otherAgain.Header().Get("ETag"))
			require.Equal(t, http.StatusNotModified, requestModelForTest(handler, other, "", oldETag).Code)
			require.Equal(t, []int64{account.ID}, upstream.accountIDs(), "lists and details in both groups must share one fresh source fetch")
		})
	}
}

func TestTiboHealthyGroupModelCodexSharedCacheAndDiscoveryPaths(t *testing.T) {
	for _, mode := range []string{"scheduled", "pinned", "pinned-scheduler-fallback"} {
		t.Run(mode, func(t *testing.T) {
			const sourceBody = `{"models":[{"slug":"gpt-6-luna"},{"slug":"gpt-5.6-luna"},{"slug":"luna"},{"slug":"openai/GPT5.6LUNA-max"},{"slug":"gpt-6-astra"},{"slug":"gpt-6-sol","context_window":424242,"retained_metadata":{"source":"codex"}},{"slug":"gpt-6.1-terra"},{"slug":"codex-auto-review"},{"slug":"gpt-image-2"}],"source_marker":"shared"}`
			account := newPinnedCodexAccount(8801, service.StatusActive, true, false)
			upstream := &codexModelsPinnedHTTPUpstream{bodies: map[int64]string{account.ID: sourceBody}}
			handler := newPinnedCodexTestHandler([]service.Account{account}, upstream, 3)
			config := service.GroupCodexModelsManifestConfig{}
			if mode != "scheduled" {
				config.Enabled = true
				config.AccountIDs = []int64{account.ID}
			}
			if mode == "pinned-scheduler-fallback" {
				config.AccountIDs = []int64{8899}
				config.FallbackToScheduler = true
			}
			other := &service.Group{
				ID: 8802, Name: service.TiboWelfareGroupName, Platform: service.PlatformOpenAI,
				ModelAllowlist:            service.GroupModelAllowlist{Enabled: true, Models: []string{"*", "codex-auto-review"}},
				CodexModelsManifestConfig: config,
			}
			healthy := &service.Group{
				ID: 8803, Name: service.TiboHealthyGroupName, Platform: service.PlatformOpenAI,
				ModelAllowlist:            service.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-luna", "codex-auto-review"}},
				CodexModelsManifestConfig: config,
			}
			firstOther := performPinnedCodexModelsRequest(t, handler, other, "")
			require.Equal(t, http.StatusOK, firstOther.Code, firstOther.Body.String())
			require.ElementsMatch(t, []string{"gpt-6-luna", "gpt-5.6-luna", "luna", "openai/GPT5.6LUNA-max", "gpt-6-astra", "gpt-6-sol", "gpt-6.1-terra", "codex-auto-review"}, codexHandlerManifestSlugs(t, firstOther))
			oldETag := firstOther.Header().Get("ETag")
			require.NotEmpty(t, oldETag)
			firstHealthy := performPinnedCodexModelsRequest(t, handler, healthy, "")
			require.Equal(t, http.StatusOK, firstHealthy.Code, firstHealthy.Body.String())
			require.ElementsMatch(t, []string{"gpt-6-luna", "gpt-5.6-luna", "luna", "openai/GPT5.6LUNA-max", "gpt-6-astra", "gpt-6-sol", "gpt-6.1-terra", "codex-auto-review"}, codexHandlerManifestSlugs(t, firstHealthy),
				"Luna, other models, and an already-enabled automatic model must remain visible")
			require.Contains(t, firstHealthy.Body.String(), `"context_window":424242`)
			require.Contains(t, firstHealthy.Body.String(), `"retained_metadata":{"source":"codex"}`)
			require.Contains(t, firstHealthy.Body.String(), `"source_marker":"shared"`)
			newETag := firstHealthy.Header().Get("ETag")
			require.Equal(t, oldETag, newETag)
			require.Equal(t, service.CodexModelsManifestETag(firstHealthy.Body.Bytes()), newETag)
			cached := performPinnedCodexModelsRequest(t, handler, healthy, newETag)
			require.Equal(t, http.StatusNotModified, cached.Code)
			require.Empty(t, cached.Body.String())
			require.Equal(t, newETag, cached.Header().Get("ETag"))
			otherAgain := performPinnedCodexModelsRequest(t, handler, other, "")
			require.Equal(t, http.StatusOK, otherAgain.Code, otherAgain.Body.String())
			require.JSONEq(t, firstOther.Body.String(), otherAgain.Body.String(), "the shared source must retain Luna for another request group")
			require.Equal(t, oldETag, otherAgain.Header().Get("ETag"))
			require.Equal(t, http.StatusNotModified, performPinnedCodexModelsRequest(t, handler, other, oldETag).Code)
			require.Equal(t, []int64{account.ID}, upstream.accountIDs(), "all three discovery routes must filter after the account cache")
		})
	}
}
