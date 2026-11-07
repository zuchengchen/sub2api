package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/stretchr/testify/require"
)

func TestNativeReasoningReplayNativeTLSProfileScope(t *testing.T) {
	t.Setenv(codexwire.NativeTLSProfileEnvironment, "openssl35-gnu")
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	t.Setenv("SSL_CERT_FILE", "")
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	profile := svc.resolveCodexHTTPSTLSProfile(account)
	require.NotNil(t, profile)
	require.Contains(t, profile.Name, "3.5.5")
	require.Equal(t, "rustls", codexWireTLSProfileName(account), "native HTTP profile must not replace WS rustls")
	account.Extra = map[string]any{"codex_tls_profile": "openssl36"}
	require.Contains(t, svc.resolveCodexHTTPSTLSProfile(account).Name, "3.6")
	account.Extra = nil
	t.Setenv("CODEX_CA_CERTIFICATE", "/synthetic/custom-ca.pem")
	profile = svc.resolveCodexHTTPSTLSProfile(account)
	require.NotNil(t, profile)
	require.True(t, profile.H2, "custom CA keeps the rustls-h2 lane")
}
