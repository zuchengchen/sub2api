package service

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/klauspost/compress/zstd"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
func (s *OpenAIGatewayService) doOpenAIUpstream(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	s.maybeCompressCodexUpstreamRequest(request, account)
	rejectedToken := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	response, err := s.doOpenAIUpstreamOnce(request, proxyURL, account)
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized || s.openAITokenProvider == nil || request.Context().Err() != nil {
		return response, err
	}
	// Only replay bodies with an explicit rewind contract, before any response
	// body has been passed to the caller or an SSE frame has been committed.
	if request.Body != nil && request.Body != http.NoBody && request.GetBody == nil {
		return response, nil
	}
	credentialAccount, err := s.openAIOAuthCredentialOwner(request.Context(), account)
	if err != nil {
		return response, nil
	}
	if !canRecoverOpenAIOAuth(credentialAccount) || rejectedToken == "" || rejectedToken == request.Header.Get("Authorization") {
		return response, nil
	}
	retry := request.Clone(request.Context())
	if request.GetBody != nil {
		retry.Body, err = request.GetBody()
		if err != nil {
			return response, nil
		}
	}
	reloadedHeaders, reloadedOwner, reloaded := s.reloadOpenAIOAuthHeaders(request.Context(), credentialAccount, request.Header)
	if !reloaded {
		if retry.Body != nil {
			_ = retry.Body.Close()
		}
		return response, nil
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	retry.Header = reloadedHeaders
	response, err = s.doOpenAIUpstreamOnce(retry, proxyURL, account)
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized || request.Context().Err() != nil {
		return response, err
	}
	credentialAccount = reloadedOwner
	rejectedToken = strings.TrimPrefix(reloadedHeaders.Get("Authorization"), "Bearer ")
	retry = request.Clone(request.Context())
	if request.GetBody != nil {
		retry.Body, err = request.GetBody()
		if err != nil {
			return response, nil
		}
	}
	token, refreshErr := s.openAITokenProvider.RefreshAfterUnauthorized(request.Context(), credentialAccount, rejectedToken)
	if refreshErr != nil {
		if retry.Body != nil && retry.Body != http.NoBody {
			_ = retry.Body.Close()
		}
		// Preserve the original 401 and its normal account error policy. A
		// temporary refresh transport failure must not become a permanent ban.
		return response, nil
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}
	retry.Header.Set("Authorization", "Bearer "+token)
	response, err = s.doOpenAIUpstreamOnce(retry, proxyURL, account)
	if err == nil && response != nil && response.StatusCode == http.StatusUnauthorized && request.URL != nil && strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/responses") {
		// A tool-specific endpoint may deny access independently of inference.
		// Only the Responses endpoint establishes rejection of the repaired auth.
		s.openAITokenProvider.RecordUnauthorizedAfterRefresh(request.Context(), credentialAccount, token)
	}
	return response, err
}

// maybeCompressCodexUpstreamRequest 复刻官方 Codex CLI 的 HTTP 请求压缩：
// enable_request_compression 默认开启（codex-rs features/lib.rs:1449），
// ChatGPT OAuth + OpenAI provider 的 Responses 请求体用 zstd level 3 压缩并
// 设置 content-encoding: zstd（http-client/request.rs:209）。
// 压缩后重写 Body/GetBody，401 重试克隆请求可复放同样的压缩字节。
func (s *OpenAIGatewayService) maybeCompressCodexUpstreamRequest(request *http.Request, account *Account) {
	if s == nil || request == nil || account == nil {
		return
	}
	// zstd 压缩只面向真正的 OpenAI Codex(ChatGPT OAuth/SetupToken)账号。
	// UsesOpenAICodexProtocol 只看 account.Type==OAuth,会把 grok/gemini/anthropic
	// 等其它平台的 OAuth 账号也判真;而 xAI 对带 content-encoding:zstd 的请求要求
	// 额外的 x-grok-model-override 头,缺头会以 400 拒绝。这里显式限定 openai 平台,
	// 避免把 Codex 专有压缩错误套到非 openai 的 OAuth 上游。
	if account.Platform != PlatformOpenAI {
		return
	}
	if !account.UsesOpenAICodexProtocol() || !s.isOpenAIWSExactFidelityEnabled() {
		return
	}
	// 官方只在 Responses HTTP executor 压缩请求体（responses_request_compression）。
	if request.URL == nil || !strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/responses") {
		return
	}
	if request.Body == nil || request.Body == http.NoBody {
		return
	}
	if strings.TrimSpace(request.Header.Get("Content-Encoding")) != "" {
		return
	}
	raw, err := io.ReadAll(request.Body)
	_ = request.Body.Close()
	if err != nil || len(raw) == 0 {
		request.Body = http.NoBody
		request.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		request.ContentLength = 0
		return
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		request.Body = io.NopCloser(bytes.NewReader(raw))
		request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
		return
	}
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		request.Body = io.NopCloser(bytes.NewReader(raw))
		request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
		return
	}
	if err := zw.Close(); err != nil {
		request.Body = io.NopCloser(bytes.NewReader(raw))
		request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
		return
	}
	compressed := buf.Bytes()
	request.Body = io.NopCloser(bytes.NewReader(compressed))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(compressed)), nil
	}
	request.ContentLength = int64(len(compressed))
	request.Header.Set("Content-Encoding", "zstd")
}

func (s *OpenAIGatewayService) doOpenAIUpstreamOnce(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			if response != nil && response.Request == nil {
				response.Request = request
			}
			return response, err
		}
	}
	response, err := s.doOpenAIHTTPTransport(WithAccountTrafficRequest(request, account), proxyURL, account, account)
	if response != nil && response.Request == nil {
		response.Request = request
	}
	return response, err
}

func (s *OpenAIGatewayService) doOpenAIHTTPTransport(request *http.Request, proxyURL string, account, credentialAccount *Account) (*http.Response, error) {
	// Resolve the HTTP lane from the credential owner's profile. Routing and
	// concurrency still belong to the selected account, including shadows.
	if credentialAccount != nil && credentialAccount.UsesOpenAICodexProtocol() && s.isOpenAIWSExactFidelityEnabled() {
		profile := s.resolveCodexHTTPSTLSProfile(credentialAccount)
		if isCodexOAuthAccount(credentialAccount) {
			var err error
			profile, err = codexwire.WithCodexCustomCA(profile)
			if err != nil {
				return nil, err
			}
		}
		return s.httpUpstream.DoWithTLS(request, proxyURL, account.ID, account.Concurrency, profile)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

// resolveCodexHTTPSTLSProfile 为 codex lane 的 HTTP 请求解析 TLS 指纹。
//
// 官方 Codex CLI（Linux）的 HTTP 出站走 reqwest default-tls → native-tls →
// 静态链接的 OpenSSL 3.6：不发 ALPN、走 HTTP/1.1、每个请求新建连接
// （codexgolden 实抓，testdata/golden/0.163.0）。因此默认档是 openssl36
// （codexwire 原始 h1 客户端）。
//
// 只有配置了自定义 CA（CODEX_CA_CERTIFICATE / SSL_CERT_FILE）时官方才切到
// rustls + ALPN [h2, http/1.1]；codex_http_h2_enabled=true 模拟这种部署
// （rustls-h2 + codexwire h2 多路复用）。账号 extra 的 codex_tls_profile /
// tls_fingerprint_builtin 优先；显式 "none" 返回 nil 走 Go 默认栈。
func (s *OpenAIGatewayService) resolveCodexHTTPSTLSProfile(account *Account) *tlsfingerprint.Profile {
	if account != nil {
		if name := strings.TrimSpace(firstNonEmpty(account.GetExtraString("codex_tls_profile"), account.GetExtraString("tls_fingerprint_builtin"))); name != "" {
			if name == "none" {
				return nil
			}
			if profile := tlsfingerprint.BuiltinProfile(name); profile != nil {
				return profile
			}
		}
	}
	if s.isCodexHTTPH2Enabled() || (isCodexOAuthAccount(account) && codexwire.CodexCustomCAConfigured()) {
		return tlsfingerprint.BuiltinProfile("rustls-h2")
	}
	return tlsfingerprint.BuiltinProfile("openssl36")
}

// isCodexHTTPH2Enabled 报告 Codex HTTP 出站是否模拟「自定义 CA」部署：
// rustls-h2 ClientHello + codexwire h2 多路复用。默认关闭（官方默认是
// OpenSSL + HTTP/1.1）。
func (s *OpenAIGatewayService) isCodexHTTPH2Enabled() bool {
	return s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.CodexHTTPH2Enabled
}

// doOpenAIAccountTestUpstream shares Codex request preparation and HTTP transport
// for OAuth-like tests while retaining each API-key endpoint's own contract.
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if account.IsOpenAI() && account.IsOpenAIOAuthLike() {
		// Keep the current sender/repository: intelligent tests wrap both to
		// capture the response and suppress account-policy writes.
		gateway := &OpenAIGatewayService{
			cfg: codexProbeConfig(s.cfg), httpUpstream: s.httpUpstream,
			accountRepo: s.accountRepo, pluginManager: s.pluginManager,
		}
		return gateway.doOpenAICodexProbe(WithAccountTrafficRequest(request, account), proxyURL, account)
	}
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			WithAccountTrafficRequest(request, account),
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(WithAccountTrafficRequest(request, account), proxyURL, account.ID, account.Concurrency)
}
