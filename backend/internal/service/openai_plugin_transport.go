package service

import (
	"bytes"
	"fmt"
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
	if err := s.maybeCompressCodexUpstreamRequest(request, account); err != nil {
		return nil, err
	}
	sentAuthorization := request.Header.Get("Authorization")
	rejectedToken := strings.TrimPrefix(sentAuthorization, "Bearer ")
	responses := request.URL != nil && strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/responses")
	sentAccount := account
	if responses {
		sentAccount = s.openAITokenProvider.accountForSentOpenAIAccess(request.Context(), account, sentAuthorization)
	}
	response, err := s.doOpenAIUpstreamOnce(request, proxyURL, account)
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized || s.openAITokenProvider == nil {
		return response, err
	}
	// A Responses 401 invalidates any proof for this bearer even if the body
	// cannot be replayed. Tool-specific 401s do not establish an inference failure.
	if responses {
		if !sameOpenAIResponsesAuthAttempt(request, response.Request, sentAuthorization) {
			return response, nil
		}
		s.openAITokenProvider.refreshAPI.rejectVerifiedOpenAIAccess(sentAccount, sentAuthorization)
	}
	if request.Context().Err() != nil {
		return response, nil
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
	if !canRecoverOpenAIOAuth(credentialAccount) || rejectedToken == "" || rejectedToken == sentAuthorization {
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
	sentAuthorization = reloadedHeaders.Get("Authorization")
	response, err = s.doOpenAIUpstreamOnce(retry, proxyURL, account)
	if err != nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}
	if responses {
		if !sameOpenAIResponsesAuthAttempt(retry, response.Request, sentAuthorization) {
			return response, nil
		}
		s.openAITokenProvider.refreshAPI.rejectVerifiedOpenAIAccess(reloadedOwner, sentAuthorization)
	}
	if request.Context().Err() != nil {
		return response, nil
	}
	credentialAccount = reloadedOwner
	rejectedToken = strings.TrimPrefix(sentAuthorization, "Bearer ")
	retry = request.Clone(request.Context())
	if request.GetBody != nil {
		retry.Body, err = request.GetBody()
		if err != nil {
			return response, nil
		}
	}
	refreshCtx := withCodexSourceBuildAuthIdentity(request.Context(), credentialAccount, request.Header)
	token, refreshErr := s.openAITokenProvider.RefreshAfterUnauthorized(refreshCtx, credentialAccount, rejectedToken)
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
	if err == nil && response != nil && response.StatusCode == http.StatusUnauthorized && responses && sameOpenAIResponsesAuthAttempt(retry, response.Request, "Bearer "+token) {
		s.openAITokenProvider.RecordUnauthorizedAfterRefresh(request.Context(), credentialAccount, token)
	}
	return response, err
}

// A redirect can change the endpoint, omit credentials, or select a different
// workspace. Only a 401 for the inference attempt itself is evidence about it.
func sameOpenAIResponsesAuthAttempt(request, final *http.Request, authorization string) bool {
	return final != nil && final.URL != nil && final.Header.Get("Authorization") == authorization &&
		final.Method == request.Method && final.URL.String() == request.URL.String() &&
		strings.EqualFold(final.Host, request.Host) &&
		final.Header.Get("chatgpt-account-id") == request.Header.Get("chatgpt-account-id")
}

// maybeCompressCodexUpstreamRequest keeps plain JSON by default. Some valid
// zstd frames receive a bare 400 from the Codex upstream while identical plain
// JSON succeeds. An explicit opt-in retains the previous level-3 encoding and
// Body/GetBody replay contract, independently of the TLS and WS settings.
func (s *OpenAIGatewayService) maybeCompressCodexUpstreamRequest(request *http.Request, account *Account) error {
	setOpenAIRequestCompressionPolicy(request, "ineligible")
	if s == nil || request == nil || account == nil {
		return nil
	}
	// zstd 压缩只面向真正的 OpenAI Codex(ChatGPT OAuth/SetupToken)账号。
	// UsesOpenAICodexProtocol 只看 account.Type==OAuth,会把 grok/gemini/anthropic
	// 等其它平台的 OAuth 账号也判真;而 xAI 对带 content-encoding:zstd 的请求要求
	// 额外的 x-grok-model-override 头,缺头会以 400 拒绝。这里显式限定 openai 平台,
	// 避免把 Codex 专有压缩错误套到非 openai 的 OAuth 上游。
	if account.Platform != PlatformOpenAI {
		return nil
	}
	if !account.UsesOpenAICodexProtocol() {
		return nil
	}
	// 官方只在 Responses HTTP executor 压缩请求体（responses_request_compression）。
	if request.URL == nil || !strings.HasSuffix(strings.TrimRight(request.URL.Path, "/"), "/responses") {
		return nil
	}
	if request.Body == nil || request.Body == http.NoBody {
		return nil
	}
	if strings.TrimSpace(request.Header.Get("Content-Encoding")) != "" {
		setOpenAIRequestCompressionPolicy(request, "existing_encoding")
		return nil
	}
	if s.cfg == nil || !s.cfg.Gateway.OpenAIRequestCompressionEnabled {
		setOpenAIRequestCompressionPolicy(request, "disabled")
		return nil
	}
	if !s.isOpenAIWSExactFidelityEnabled() {
		return nil
	}
	raw, err := io.ReadAll(request.Body)
	_ = request.Body.Close()
	if err != nil {
		return fmt.Errorf("read Codex request body for compression: %w", err)
	}
	if len(raw) == 0 {
		request.Body = http.NoBody
		request.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		request.ContentLength = 0
		return nil
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return fmt.Errorf("prepare Codex request compression: %w", err)
	}
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return fmt.Errorf("compress Codex request body: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finish Codex request compression: %w", err)
	}
	compressed := buf.Bytes()
	request.Body = io.NopCloser(bytes.NewReader(compressed))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(compressed)), nil
	}
	request.ContentLength = int64(len(compressed))
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Content-Encoding", "zstd")
	setOpenAIRequestCompressionPolicy(request, "zstd")
	return nil
}

func (s *OpenAIGatewayService) doOpenAIUpstreamOnce(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if err := checkNativeReasoningRequestCredential(request); err != nil {
		return nil, err
	}
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			if response != nil && response.Request == nil {
				response.Request = request
			}
			return response, err
		}
	}
	response, err := s.doOpenAIHTTPTransport(request, proxyURL, account, account)
	if response != nil && response.Request == nil {
		response.Request = request
	}
	return response, err
}

func (s *OpenAIGatewayService) doOpenAIHTTPTransport(request *http.Request, proxyURL string, account, credentialAccount *Account) (*http.Response, error) {
	// Each OAuth API attempt builds a fresh official HTTP client. Scope the
	// marker to this clone, including explicitly clearing it on other accounts.
	if business := codexBusinessHTTPAccount(credentialAccount); business || CodexBusinessHTTPFromContext(request.Context()) {
		request = request.Clone(WithCodexBusinessHTTP(request.Context(), business))
	}
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
	return codexwire.NativeHTTPProfile()
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
		return s.codexProbeGateway(s.pluginManager).doOpenAICodexProbe(request, proxyURL, account)
	}
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

func (s *AccountTestService) codexProbeGateway(plugins *PluginManager) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg: codexProbeConfig(s.cfg), httpUpstream: s.httpUpstream,
		accountRepo: s.accountRepo, pluginManager: plugins,
	}
}
