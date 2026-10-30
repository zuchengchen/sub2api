package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type codexModelsTransportRecorder struct {
	httpUpstreamRecorder
	profile *tlsfingerprint.Profile
}

func (u *codexModelsTransportRecorder) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.profile = profile
	return u.Do(req, proxy, accountID, concurrency)
}

func TestAdminCodexModelsUsesAccountIdentityAndTransport(t *testing.T) {
	account := directImagesTestAccount()
	account.Extra = map[string]any{codexClientFamilyExtraKey: codexClientFamilyExec}
	upstream := &codexModelsTransportRecorder{}
	cfg := codexProbeConfig(nil)
	svc := &AccountTestService{cfg: cfg, httpUpstream: upstream}
	req, err := svc.buildOpenAIOAuthUpstreamModelsRequest(context.Background(), account)
	require.NoError(t, err)
	profile, ok := resolveCodexDeviceProfile(account)
	require.True(t, ok)
	require.Equal(t, profile.UserAgent, req.Header.Get("User-Agent"))
	require.Equal(t, "codex_exec", req.Header.Get("Originator"))
	require.Equal(t, req.Header.Get("Version"), req.URL.Query().Get("client_version"))
	require.Equal(t, "*/*", req.Header.Get("Accept"))
	_, err = svc.doUpstreamModelsRequest(req, "", account)
	require.NoError(t, err)
	require.NotNil(t, upstream.profile)
	require.True(t, upstream.profile.H1)
	cfg.Gateway.OpenAIWS.CodexHTTPH2Enabled = true
	_, err = svc.doUpstreamModelsRequest(req, "", account)
	require.NoError(t, err)
	require.True(t, upstream.profile.H2)
}

func TestCodex166GuardianRecordedItemsOrder(t *testing.T) {
	body, err := marshalCodexTurnMetadata(map[string]any{"guardian_recorded_items": true, "installation_id": "installation", "thread_source": "user"})
	require.NoError(t, err)
	require.Equal(t, `{"guardian_recorded_items":true,"installation_id":"installation","thread_source":"user"}`, string(body))
	body, err = marshalCodexTurnMetadata(map[string]any{"thread_source": "user"})
	require.NoError(t, err)
	require.Equal(t, `{"thread_source":"user"}`, string(body), "do not claim guardian recording when absent")
}

func TestClientCodexModelsRetainsAccountFamilyAndBootstrapStage(t *testing.T) {
	account := directImagesTestAccount()
	account.Extra = map[string]any{codexClientFamilyExtraKey: codexClientFamilyExec}
	version := CodexCanonicalClientVersion()
	profile, ok := resolveCodexDeviceProfile(account)
	require.True(t, ok)
	for _, bootstrap := range []bool{true, false} {
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-5.5"}]}`))}}
		svc := &OpenAIGatewayService{cfg: codexProbeConfig(nil), httpUpstream: upstream}
		ua := profile.UserAgent
		if bootstrap {
			ua = strings.TrimSuffix(ua, " (codex_exec; "+version+")")
		}
		ctx := WithCodexModelsClientContext(context.Background(), ua, "codex_exec")
		_, err := svc.FetchCodexModelsManifest(ctx, account, version, "")
		require.NoError(t, err)
		require.Equal(t, "codex_exec", upstream.lastReq.Header.Get("Originator"))
		require.Equal(t, ua, upstream.lastReq.Header.Get("User-Agent"))
	}
}

func BenchmarkCodexAuxiliaryRequests(b *testing.B) {
	b.Run("models", func(b *testing.B) {
		svc := &AccountTestService{cfg: codexProbeConfig(nil)}
		account := directImagesTestAccount()
		account.Extra = map[string]any{codexClientFamilyExtraKey: codexClientFamilyExec}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if req, err := svc.buildOpenAIOAuthUpstreamModelsRequest(context.Background(), account); err != nil || req.Header.Get("Authorization") == "" {
				b.Fatal("models request failed", err)
			}
		}
	})
	b.Run("turn_metadata", func(b *testing.B) {
		metadata := map[string]any{"installation_id": "test-installation", "session_id": "test-session", "thread_id": "test-session", "turn_id": "test-turn", "thread_source": "user", "turn_trigger": "exec", "sandbox": "seccomp", "model": "gpt-5.5"}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if data, err := marshalCodexTurnMetadata(metadata); err != nil || len(data) == 0 {
				b.Fatal("metadata failed", err)
			}
		}
	})
}
