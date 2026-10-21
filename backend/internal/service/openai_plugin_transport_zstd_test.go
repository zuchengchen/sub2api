package service

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/klauspost/compress/zstd"
)

func zstdTestCodexAccount(t *testing.T) *Account {
	t.Helper()
	return &Account{
		ID:       42,
		Type:     AccountTypeOAuth,
		Platform: PlatformOpenAI,
		Extra:    map[string]any{},
	}
}

func TestMaybeCompressCodexUpstreamRequest_CompressesResponses(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: codexExactFidelityTestConfig(t)}
	account := zstdTestCodexAccount(t)

	plain := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	svc.maybeCompressCodexUpstreamRequest(req, account)

	if got := req.Header.Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("Content-Encoding = %q, want zstd", got)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if int64(len(body)) != req.ContentLength {
		t.Fatalf("ContentLength = %d, body %d", req.ContentLength, len(body))
	}
	zr, err := zstd.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer zr.Close()
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decompressed body mismatch:\n got: %s\nwant: %s", got, plain)
	}

	// 401 重放：GetBody 必须返回相同的压缩字节。
	rc, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody: %v", err)
	}
	replay, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("replay ReadAll: %v", err)
	}
	if !bytes.Equal(replay, body) {
		t.Fatal("GetBody replay bytes differ from first Body read")
	}
}

func TestMaybeCompressCodexUpstreamRequest_SkipsNonResponsesPath(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: codexExactFidelityTestConfig(t)}
	account := zstdTestCodexAccount(t)

	for _, path := range []string{
		"https://chatgpt.com/backend-api/codex/responses/count_tokens",
		"https://chatgpt.com/backend-api/codex/images/generations",
		"https://chatgpt.com/v1/embeddings",
	} {
		plain := []byte(`{"a":1}`)
		req, err := http.NewRequest(http.MethodPost, path, bytes.NewReader(plain))
		if err != nil {
			t.Fatalf("NewRequest %s: %v", path, err)
		}
		svc.maybeCompressCodexUpstreamRequest(req, account)
		if got := req.Header.Get("Content-Encoding"); got != "" {
			t.Fatalf("%s: Content-Encoding = %q, want empty", path, got)
		}
		body, _ := io.ReadAll(req.Body)
		if !bytes.Equal(body, plain) {
			t.Fatalf("%s: body mutated", path)
		}
	}
}

func TestMaybeCompressCodexUpstreamRequest_SkipsNonCodexAndDisabled(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: codexExactFidelityTestConfig(t)}

	apiKeyAccount := &Account{ID: 1, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}
	plain := []byte(`{"model":"gpt-5","input":[]}`)

	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	svc.maybeCompressCodexUpstreamRequest(req, apiKeyAccount)
	if got := req.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("api-key account Content-Encoding = %q", got)
	}

	// 非 openai 平台的 OAuth 账号（如 grok）：UsesOpenAICodexProtocol 只按 Type 判真，
	// 但 zstd 是 Codex 专有压缩，xAI 等上游会 400 拒绝带它的请求。
	grokOAuth := &Account{ID: 2, Type: AccountTypeOAuth, Platform: PlatformGrok, Extra: map[string]any{}}
	reqGrok, err := http.NewRequest(http.MethodPost, "https://api.x.ai/v1/responses", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	svc.maybeCompressCodexUpstreamRequest(reqGrok, grokOAuth)
	if got := reqGrok.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("grok oauth account Content-Encoding = %q, want empty", got)
	}

	// exact fidelity 关闭时，codex 账号也不压缩。
	svcOff := &OpenAIGatewayService{cfg: codexExactFidelityOffTestConfig(t)}
	req2, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	svcOff.maybeCompressCodexUpstreamRequest(req2, zstdTestCodexAccount(t))
	if got := req2.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("disabled config Content-Encoding = %q", got)
	}
}

func TestMaybeCompressCodexUpstreamRequest_PreservesExistingEncoding(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: codexExactFidelityTestConfig(t)}
	account := zstdTestCodexAccount(t)

	plain := []byte(`{"model":"gpt-5"}`)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Encoding", "gzip")
	svc.maybeCompressCodexUpstreamRequest(req, account)
	if got := req.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	body, _ := io.ReadAll(req.Body)
	if !bytes.Equal(body, plain) {
		t.Fatal("body mutated despite existing Content-Encoding")
	}
}

func TestResolveCodexHTTPSTLSProfile_DefaultsAndOverrides(t *testing.T) {
	svc := &OpenAIGatewayService{}

	if p := svc.resolveCodexHTTPSTLSProfile(zstdTestCodexAccount(t)); p == nil || !strings.Contains(strings.ToLower(p.Name), "rustls") {
		t.Fatalf("default profile = %+v, want rustls", p)
	}

	withProfile := zstdTestCodexAccount(t)
	withProfile.Extra["codex_tls_profile"] = "nodejs24"
	if p := svc.resolveCodexHTTPSTLSProfile(withProfile); p == nil || !strings.Contains(strings.ToLower(p.Name), "node") {
		t.Fatalf("override profile = %+v, want nodejs", p)
	}

	withNone := zstdTestCodexAccount(t)
	withNone.Extra["codex_tls_profile"] = "none"
	if p := svc.resolveCodexHTTPSTLSProfile(withNone); p != nil {
		t.Fatalf("none profile = %+v, want nil", p)
	}
}

// codexExactFidelityTestConfig 构造开启 exact fidelity 的测试配置。
func codexExactFidelityTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
	return cfg
}

func codexExactFidelityOffTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{}
}
