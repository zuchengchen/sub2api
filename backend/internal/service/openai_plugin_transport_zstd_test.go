package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
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
	svc := &OpenAIGatewayService{cfg: codexCompressionOptInTestConfig(t)}
	account := zstdTestCodexAccount(t)

	plain := []byte(`{"model":"gpt-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	require.NoError(t, svc.maybeCompressCodexUpstreamRequest(req, account))

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
	svc := &OpenAIGatewayService{cfg: codexCompressionOptInTestConfig(t)}
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
		require.NoError(t, svc.maybeCompressCodexUpstreamRequest(req, account))
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
	svc := &OpenAIGatewayService{cfg: codexCompressionOptInTestConfig(t)}

	apiKeyAccount := &Account{ID: 1, Type: AccountTypeAPIKey, Platform: PlatformOpenAI}
	plain := []byte(`{"model":"gpt-5","input":[]}`)

	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	require.NoError(t, svc.maybeCompressCodexUpstreamRequest(req, apiKeyAccount))
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
	require.NoError(t, svc.maybeCompressCodexUpstreamRequest(reqGrok, grokOAuth))
	if got := reqGrok.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("grok oauth account Content-Encoding = %q, want empty", got)
	}

	// exact fidelity 关闭时，codex 账号也不压缩。
	disabledFidelity := codexCompressionOptInTestConfig(t)
	disabledFidelity.Gateway.OpenAIWS.ExactFidelityEnabled = false
	svcOff := &OpenAIGatewayService{cfg: disabledFidelity}
	req2, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	require.NoError(t, svcOff.maybeCompressCodexUpstreamRequest(req2, zstdTestCodexAccount(t)))
	if got := req2.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("disabled config Content-Encoding = %q", got)
	}
}

func TestMaybeCompressCodexUpstreamRequest_PreservesExistingEncoding(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: codexCompressionOptInTestConfig(t)}
	account := zstdTestCodexAccount(t)

	plain := []byte(`{"model":"gpt-5"}`)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Encoding", "gzip")
	require.NoError(t, svc.maybeCompressCodexUpstreamRequest(req, account))
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

	// Official Linux Codex CLI HTTP lane: OpenSSL, no ALPN, raw HTTP/1.1.
	if p := svc.resolveCodexHTTPSTLSProfile(zstdTestCodexAccount(t)); p == nil || !p.H1 || p.H2 || len(p.ALPNProtocols) != 0 || !strings.Contains(p.Name, "OpenSSL") {
		t.Fatalf("default profile = %+v, want openssl36 h1", p)
	}

	// Custom-CA deployment emulation: rustls + h2.
	h2Svc := &OpenAIGatewayService{cfg: &config.Config{}}
	h2Svc.cfg.Gateway.OpenAIWS.CodexHTTPH2Enabled = true
	if p := h2Svc.resolveCodexHTTPSTLSProfile(zstdTestCodexAccount(t)); p == nil || !p.H2 || !strings.Contains(strings.ToLower(p.Name), "rustls") {
		t.Fatalf("h2 profile = %+v, want rustls-h2", p)
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

func codexCompressionOptInTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := codexExactFidelityTestConfig(t)
	cfg.Gateway.OpenAIRequestCompressionEnabled = true
	return cfg
}

type codexCompressionObservedBody struct {
	io.Reader
	reads  int
	closed bool
}

func (b *codexCompressionObservedBody) Read(p []byte) (int, error) {
	b.reads++
	return b.Reader.Read(p)
}

func (b *codexCompressionObservedBody) Close() error {
	b.closed = true
	return nil
}

func TestMaybeCompressCodexUpstreamRequest_DefaultPreservesRawBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
	}{
		{name: "nil config"},
		{name: "zero config", cfg: &config.Config{}},
		{name: "exact fidelity remains enabled", cfg: codexExactFidelityTestConfig(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Exercise a multi-block body with UTF-8, escaped text and exact whitespace.
			plain := []byte(`{ "model":"gpt-6-astra", "input":"中文 & <tag> \n` + strings.Repeat("x", 131073) + `", "store":false }`)
			req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, bytes.NewReader(plain))
			require.NoError(t, err)
			observed := &codexCompressionObservedBody{Reader: req.Body}
			req.Body = observed
			req.Header.Set("Content-Type", "application/json")
			svc := &OpenAIGatewayService{cfg: tc.cfg}
			require.NoError(t, svc.maybeCompressCodexUpstreamRequest(req, zstdTestCodexAccount(t)))
			require.Same(t, observed, req.Body, "default path must not buffer or replace the body")
			require.Zero(t, observed.reads)
			require.False(t, observed.closed)
			require.Empty(t, req.Header.Get("Content-Encoding"))
			require.Equal(t, "application/json", req.Header.Get("Content-Type"))
			require.Equal(t, int64(len(plain)), req.ContentLength)
			got, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.True(t, bytes.Equal(plain, got), "default body bytes changed")
			replay, err := req.GetBody()
			require.NoError(t, err)
			defer replay.Close()
			got, err = io.ReadAll(replay)
			require.NoError(t, err)
			require.True(t, bytes.Equal(plain, got), "raw GetBody replay bytes changed")
		})
	}
}

func TestMaybeCompressCodexUpstreamRequest_DefaultDoesNotReadLargeBody(t *testing.T) {
	readErr := errors.New("the default compression policy must not read this body")
	body := &codexCompressionObservedBody{Reader: &codexCompressionReadError{err: readErr}}
	req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, body)
	require.NoError(t, err)
	req.ContentLength = 85327387
	svc := &OpenAIGatewayService{cfg: codexExactFidelityTestConfig(t)}
	require.NoError(t, svc.maybeCompressCodexUpstreamRequest(req, zstdTestCodexAccount(t)))
	require.Same(t, body, req.Body)
	require.EqualValues(t, 85327387, req.ContentLength)
	require.Zero(t, body.reads)
	require.False(t, body.closed)
	require.Nil(t, req.GetBody, "default policy must preserve a non-rewindable stream")
}

func TestMaybeCompressCodexUpstreamRequest_OptInBlockBoundaries(t *testing.T) {
	for _, size := range []int{131071, 131072, 131073, 656595} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			prefix := []byte(`{"model":"gpt-6-astra","input":"`)
			plain := append(prefix, bytes.Repeat([]byte("x"), size-len(prefix)-2)...)
			plain = append(plain, '"', '}')
			require.Len(t, plain, size)
			req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, bytes.NewReader(plain))
			require.NoError(t, err)
			svc := &OpenAIGatewayService{cfg: codexCompressionOptInTestConfig(t)}
			require.NoError(t, svc.maybeCompressCodexUpstreamRequest(req, zstdTestCodexAccount(t)))
			require.Equal(t, "zstd", req.Header.Get("Content-Encoding"))
			wire, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.Equal(t, int64(len(wire)), req.ContentLength)
			decoder, err := zstd.NewReader(nil)
			require.NoError(t, err)
			defer decoder.Close()
			decoded, err := decoder.DecodeAll(wire, nil)
			require.NoError(t, err)
			require.True(t, bytes.Equal(plain, decoded), "opt-in compression changed JSON bytes")
			replay, err := req.GetBody()
			require.NoError(t, err)
			defer replay.Close()
			replayed, err := io.ReadAll(replay)
			require.NoError(t, err)
			require.True(t, bytes.Equal(wire, replayed), "compressed replay differs from first send")
		})
	}
}

type codexCompressionReadError struct{ err error }

func (r *codexCompressionReadError) Read(p []byte) (int, error) {
	return copy(p, `{"partial":`), r.err
}

func TestOpenAIUpstream_CompressionReadErrorNeverSends(t *testing.T) {
	sourceErr := errors.New("synthetic body read failure")
	body := &codexCompressionObservedBody{Reader: &codexCompressionReadError{err: sourceErr}}
	req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, body)
	require.NoError(t, err)
	req.ContentLength = 100
	// A nil upstream intentionally panics if preparation wrongly reaches transport.
	svc := &OpenAIGatewayService{cfg: codexCompressionOptInTestConfig(t)}
	resp, err := svc.doOpenAIUpstream(req, "", zstdTestCodexAccount(t))
	require.Nil(t, resp)
	require.ErrorIs(t, err, sourceErr)
	require.True(t, body.closed)
	require.Empty(t, req.Header.Get("Content-Encoding"))
	require.EqualValues(t, 100, req.ContentLength, "a read failure must not become an empty POST")
}
