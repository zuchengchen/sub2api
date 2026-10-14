package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsesCustomOpenAICompatibleUpstream(t *testing.T) {
	require.False(t, usesCustomOpenAICompatibleUpstream(nil))
	require.False(t, usesCustomOpenAICompatibleUpstream(&Account{
		Platform: PlatformOpenAI, Type: AccountTypeOAuth,
	}))
	require.False(t, usesCustomOpenAICompatibleUpstream(&Account{
		Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
	}))
	require.False(t, usesCustomOpenAICompatibleUpstream(&Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://api.openai.com/v1"},
	}))
	require.True(t, usesCustomOpenAICompatibleUpstream(&Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "http://127.0.0.1:18083"},
	}))
}

func TestOpenAIValidateOutboundURLAllowsLoopbackHTTP(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:       true,
				UpstreamHosts: []string{"api.openai.com"},
			},
		},
	}}

	normalized, err := svc.validateOutboundURL("http://127.0.0.1:18083")
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:18083", normalized)

	_, err = svc.validateOutboundURL("http://192.168.1.10:18083")
	require.Error(t, err)

	_, err = svc.validateOutboundURL("https://evil.example")
	require.Error(t, err)
}

func TestAccountTestServiceValidateUpstreamBaseURLAllowsLoopbackHTTP(t *testing.T) {
	svc := &AccountTestService{cfg: &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:       true,
				UpstreamHosts: []string{"api.openai.com"},
			},
		},
	}}

	normalized, err := svc.validateUpstreamBaseURL("http://127.0.0.1:18083")
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:18083", normalized)
}

func TestOpenAIBuildUpstreamRequestCustomHopForwardsSessionHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("session-id", "conv-1")
	c.Request.Header.Set("x-session-id", "xsess-1")
	c.Request.Header.Set("x-conversation-id", "xconv-1")
	c.Request.Header.Set("thread-id", "thread-1")
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.0")
	c.Request.Header.Set("X-Unrelated", "drop-me")

	svc := &OpenAIGatewayService{}
	hop := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-vm-test",
			"base_url": "http://127.0.0.1:18083",
		},
		Extra: map[string]any{"openai_responses_supported": true},
	}

	req, err := svc.buildUpstreamRequest(context.Background(), c, hop, body, "sk-vm-test", true, "", false)
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:18083/v1/responses", req.URL.String())
	require.Equal(t, "conv-1", req.Header.Get("session-id"))
	require.Equal(t, "xsess-1", req.Header.Get("x-session-id"))
	require.Equal(t, "xconv-1", req.Header.Get("x-conversation-id"))
	require.Equal(t, "thread-1", req.Header.Get("thread-id"))
	require.Equal(t, openaiCompatibleDefaultUserAgent, req.Header.Get("User-Agent"))
	require.Empty(t, req.Header.Get("X-Unrelated"))
}

func TestOpenAIBuildUpstreamRequestOfficialAPIKeyOmitsSessionHyphenHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("session-id", "conv-1")
	c.Request.Header.Set("x-session-id", "xsess-1")
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.0")

	svc := &OpenAIGatewayService{}
	official := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-openai",
			"base_url": "https://api.openai.com/v1",
		},
		Extra: map[string]any{"openai_responses_supported": true},
	}

	req, err := svc.buildUpstreamRequest(context.Background(), c, official, body, "sk-openai", true, "", false)
	require.NoError(t, err)
	require.Equal(t, "https://api.openai.com/v1/responses", req.URL.String())
	require.Empty(t, req.Header.Get("session-id"))
	require.Empty(t, req.Header.Get("x-session-id"))
	require.Equal(t, "claude-cli/2.1.0", req.Header.Get("User-Agent"))
}

func TestCopyAllowedOpenAIClientHeadersRawChatAddsSessionOnCustomHop(t *testing.T) {
	src := http.Header{}
	src.Set("User-Agent", "curl/8.0")
	src.Set("session-id", "conv-cc")
	src.Set("Accept-Language", "en")

	hop := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "http://127.0.0.1:18083"},
	}
	dst := http.Header{}
	copyAllowedOpenAIClientHeaders(dst, src, openaiCCRawAllowedHeaders, hop)
	require.Equal(t, "conv-cc", dst.Get("session-id"))
	require.Equal(t, "en", dst.Get("Accept-Language"))

	official := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "https://api.openai.com"},
	}
	officialDst := http.Header{}
	copyAllowedOpenAIClientHeaders(officialDst, src, openaiCCRawAllowedHeaders, official)
	require.Empty(t, officialDst.Get("session-id"))
	require.Equal(t, "en", officialDst.Get("Accept-Language"))
}

func TestOpenAIPassthroughCustomHopForwardsSessionHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("session-id", "conv-pt")
	c.Request.Header.Set("User-Agent", "claude-cli/2.1.0")

	svc := &OpenAIGatewayService{}
	hop := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-vm-test",
			"base_url": "http://127.0.0.1:18083",
		},
		Extra: map[string]any{"openai_passthrough": true, "openai_responses_supported": true},
	}

	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, hop, body, "sk-vm-test")
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:18083/v1/responses", req.URL.String())
	require.Equal(t, "conv-pt", req.Header.Get("session-id"))
	require.Equal(t, openaiCompatibleDefaultUserAgent, req.Header.Get("User-Agent"))
}
