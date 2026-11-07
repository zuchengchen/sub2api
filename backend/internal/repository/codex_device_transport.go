package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/imroc/req/v3"
)

// codexDeviceClientDisabledEnv restores the legacy clients (Go TLS for OAuth
// token calls, req Firefox impersonation for chatgpt.com backend-api) when set
// to "1"/"true". Rollback switch only.
const codexDeviceClientDisabledEnv = "SUB2API_CODEX_DEVICE_CLIENT_DISABLED"

func codexDeviceClientDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(codexDeviceClientDisabledEnv)))
	return v == "1" || v == "true" || v == "yes"
}

// Header orders captured from codex-cli 0.163.0 (backend/tools/codexgolden,
// internal/pkg/codexwire/testdata/golden/0.163.0). Names not listed are
// appended lowercase after the template; hyper writes host and
// content-length last.
var (
	// POST auth.openai.com/oauth/token (login/src/auth/manager.rs refresh).
	codexDeviceTokenHeaderOrder = []string{"content-type", "originator", "user-agent", "accept"}
	// GET {chatgpt}/backend-api/codex/models (provider default headers).
	codexDeviceModelsHeaderOrder = []string{"version", "authorization", "chatgpt-account-id", "x-openai-fedramp", "originator", "user-agent", "accept"}
	// backend-client calls: wham/usage, wham/accounts/check, reset credits…
	// (backend-client/src/client.rs headers()).
	codexDeviceBackendHeaderOrder = []string{"user-agent", "authorization", "chatgpt-account-id", "x-openai-fedramp", "x-openai-codex-luna-reserve", "content-type", "accept"}
)

// codexDeviceDroppedHeaders are added by Go/req or by legacy browser
// impersonation and are never sent by the Codex CLI on these calls.
var codexDeviceDroppedHeaders = map[string]bool{
	"accept-encoding": true,
	"accept-language": true,
	"connection":      true,
	"origin":          true,
	"referer":         true,
	"sec-fetch-mode":  true,
	"sec-fetch-site":  true,
	"sec-fetch-dest":  true,
	"sec-ch-ua":       true,
	"priority":        true,
	"te":              true,
	"oai-language":    true,
	"openai-beta":     true,
}

func codexDeviceHeaderOrderFor(u string) []string {
	switch {
	case strings.Contains(u, "/oauth/token"):
		return codexDeviceTokenHeaderOrder
	case strings.Contains(u, "/codex/models"):
		return codexDeviceModelsHeaderOrder
	default:
		return codexDeviceBackendHeaderOrder
	}
}

// codexDeviceOrderedHeaders flattens h into the Codex CLI wire order for the
// request URL: lowercase names, legacy/browser headers removed, reqwest's
// default accept (*/*) when the caller did not set one.
func codexDeviceOrderedHeaders(rawURL string, h http.Header) []codexwire.HeaderKV {
	template := codexDeviceHeaderOrderFor(rawURL)
	used := map[string]bool{}
	var out []codexwire.HeaderKV
	get := func(name string) []string {
		for k, v := range h {
			if strings.EqualFold(k, name) {
				used[strings.ToLower(k)] = true
				return v
			}
		}
		return nil
	}
	for _, name := range template {
		values := get(name)
		if name == "accept" && len(values) == 0 {
			values = []string{"*/*"}
		}
		for _, v := range values {
			if strings.TrimSpace(v) != "" {
				out = append(out, codexwire.HeaderKV{Name: name, Value: v})
			}
		}
	}
	for k, values := range h {
		name := strings.ToLower(k)
		if used[name] || codexDeviceDroppedHeaders[name] || name == "host" || name == "content-length" || name == "transfer-encoding" {
			continue
		}
		for _, v := range values {
			if strings.TrimSpace(v) != "" {
				out = append(out, codexwire.HeaderKV{Name: name, Value: v})
			}
		}
	}
	return out
}

// codexDeviceRoundTripper sends requests the way the Codex CLI's default
// reqwest client does on Linux: OpenSSL-profile ClientHello without ALPN,
// HTTP/1.1, captured header order, one connection per request.
type codexDeviceRoundTripper struct {
	proxyURL  string
	profile   *tlsfingerprint.Profile
	timeout   time.Duration
	authH2    bool
	authCode  bool
	codeH1    *codexwire.H1Pool
	codeH2    *codexwire.H2Pool
	quotaOnce sync.Once
	quotaH1   *codexwire.H1Pool
	quotaH2   *codexwire.H2Pool
}

// Auth clients share only the transport pool; the key includes the exact
// credentials, device identity and trust configuration. Failed refreshes may
// reuse the client; a successful auth reload retires that generation.
var codexAuthH2Pool = codexwire.NewH2Pool(codexwire.H2Options{Timeout: 35 * time.Second})

func codexAuthPoolKey(ctx context.Context, rawURL string, headers http.Header, body []byte) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) == nil && fields != nil {
		delete(fields, "refresh_reason") // trigger is not an auth-client identity
		if canonical, err := json.Marshal(fields); err == nil {
			body = canonical
		}
	}
	identity, _ := json.Marshal([]string{strconv.FormatInt(openai.CodexAuthAccountFromContext(ctx), 10), rawURL, headers.Get("User-Agent"), headers.Get("originator"), string(body)})
	if id := openai.CodexAuthAccountFromContext(ctx); id > 0 {
		return fmt.Sprintf("auth-account-%d|%x", id, sha256.Sum256(identity))
	}
	return fmt.Sprintf("auth-%x", sha256.Sum256(identity))
}

func (t *codexDeviceRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		b, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	headers := r.Header
	if t.authCode {
		// A login client is built without the AuthManager's product headers.
		// Also omit req's default UA if an outer caller installed one.
		headers = headers.Clone()
		for name := range headers {
			if strings.EqualFold(name, "User-Agent") || strings.EqualFold(name, "Originator") {
				delete(headers, name)
			}
		}
	}
	ordered := codexDeviceOrderedHeaders(r.URL.String(), headers)
	profile := t.profile
	var pool *codexwire.H2Pool
	var h1Pool *codexwire.H1Pool
	var identity string
	if t.authCode {
		identity = "login" // pool is owned by this single login client
		if t.authH2 {
			pool = t.codeH2
		} else {
			h1Pool = t.codeH1
		}
	} else if t.authH2 {
		pool = codexAuthH2Pool
		identity = codexAuthPoolKey(r.Context(), r.URL.String(), r.Header, body)
	} else if isCodexQuotaRequest(r) {
		identity = codexQuotaPoolKey(r.Context(), r.Header)
		if codexwire.CodexCustomCAConfigured() {
			var err error
			profile, err = codexwire.WithCodexCustomCA(tlsfingerprint.BuiltinProfile("rustls-h2"))
			if err != nil {
				return nil, err
			}
			pool = t.quotaHTTP2Pool()
		} else {
			h1Pool = t.quotaHTTP1Pool()
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	var resp *http.Response
	var err error
	if pool != nil {
		if len(body) > 0 || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			ordered = append(ordered, codexwire.HeaderKV{Name: "content-length", Value: strconv.Itoa(len(body))})
		}
		resp, err = pool.Do(ctx, r.Method, r.URL.String(), ordered, body, t.proxyURL, identity, profile)
	} else if h1Pool != nil {
		resp, err = h1Pool.Do(ctx, r.Method, r.URL.String(), ordered, body, t.proxyURL, identity, codexwire.HTTP1Options{
			TLSProfile: profile, DialTimeout: defaultUpstreamDialTimeout + 5*time.Second, ResponseHeaderTimeout: t.timeout,
		})
	} else {
		resp, err = codexwire.DoHTTP1(ctx, r.Method, r.URL.String(), ordered, body, t.proxyURL, codexwire.HTTP1Options{
			TLSProfile:            t.profile,
			DialTimeout:           defaultUpstreamDialTimeout + 5*time.Second,
			ResponseHeaderTimeout: t.timeout,
		})
	}
	if err != nil {
		cancel()
		return nil, err
	}
	decompressResponseBody(resp)
	resp.Request = r
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// newCodexDeviceReqClient returns a req client whose transport is the Codex
// device lane, so existing req-based callers keep their API while their
// traffic matches the Codex CLI.
func newCodexDeviceReqClient(proxyURL string, timeout time.Duration) (*req.Client, error) {
	return newCodexDeviceReqClientWithAuthCA(proxyURL, timeout, false)
}

func newCodexDeviceReqClientWithAuthCA(proxyURL string, timeout time.Duration, auth bool) (*req.Client, error) {
	return newCodexDeviceReqClientConfigured(proxyURL, timeout, auth, false)
}

// One explicit login client, never an AuthManager credential generation or a
// process-wide token pool. It may reuse its own socket for related login grants.
func newCodexAuthCodeReqClient(proxyURL string, timeout time.Duration) (*req.Client, error) {
	return newCodexDeviceReqClientConfigured(proxyURL, timeout, false, true)
}

func newCodexDeviceReqClientConfigured(proxyURL string, timeout time.Duration, auth, authCode bool) (*req.Client, error) {
	trimmed, _, err := proxyurl.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	rt := &codexDeviceRoundTripper{proxyURL: trimmed, profile: codexwire.NativeHTTPProfile(), timeout: timeout}
	if authCode {
		rt.authCode = true
		rt.codeH1 = codexwire.NewH1Pool(codexwire.H1PoolOptions{})
		rt.codeH2 = codexwire.NewH2Pool(codexwire.H2Options{Timeout: 35 * time.Second, LogKey: "codex-login"})
		if codexwire.CodexCustomCAConfigured() {
			profile, caErr := codexwire.WithCodexCustomCA(tlsfingerprint.BuiltinProfile("rustls-h2"))
			if caErr != nil {
				return nil, caErr
			}
			rt.profile, rt.authH2 = profile, true
		}
	}
	if auth && codexwire.CodexCustomCAConfigured() {
		profile, caErr := codexwire.WithCodexCustomCA(tlsfingerprint.BuiltinProfile("rustls-h2"))
		if caErr == nil {
			rt.profile, rt.authH2 = profile, true
		}
		// The default Codex auth builder retries its original native/system-root
		// builder when custom-CA construction fails. Runtime TLS failures never
		// trigger this fallback, and certificate verification remains enabled.
	}
	client := req.C().SetTimeout(timeout)
	client.GetTransport().WrapRoundTripFunc(func(http.RoundTripper) req.HttpRoundTripFunc {
		return rt.RoundTrip
	})
	return instrumentReqClient(client), nil
}
