package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexwire"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// These are backend-client Codex endpoints. The shared privacy factory also
// handles web account/settings calls, whose existing trust behavior is retained.
func isCodexQuotaRequest(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	switch req.URL.Path {
	case "/backend-api/wham/usage", "/backend-api/wham/rate-limit-reset-credits":
		return req.Method == http.MethodGet
	case "/backend-api/wham/rate-limit-reset-credits/consume":
		return req.Method == http.MethodPost
	default:
		return false
	}
}

func (t *codexDeviceRoundTripper) quotaHTTP2Pool() *codexwire.H2Pool {
	t.quotaOnce.Do(func() {
		t.quotaH2 = codexwire.NewH2Pool(codexwire.H2Options{Timeout: 35 * time.Second, LogKey: "codex-quota"})
		t.quotaH1 = codexwire.NewH1Pool(codexwire.H1PoolOptions{})
	})
	return t.quotaH2
}

func (t *codexDeviceRoundTripper) quotaHTTP1Pool() *codexwire.H1Pool {
	_ = t.quotaHTTP2Pool()
	return t.quotaH1
}

// A req client may be shared by proxy. Its HTTP/2 sessions must still belong to
// one effective OAuth snapshot. The fingerprint stays in memory; H2Options.LogKey
// replaces it in diagnostics. Proxy, host and CA identity are added by H2Pool.
func codexQuotaPoolKey(ctx context.Context, headers http.Header) string {
	identity, _ := json.Marshal([]string{
		strconv.FormatInt(openai.CodexAuthAccountFromContext(ctx), 10),
		headers.Get("Authorization"), headers.Get("ChatGPT-Account-Id"),
		headers.Get("X-OpenAI-Fedramp"), headers.Get("User-Agent"),
	})
	fingerprint := sha256.Sum256(identity)
	return hex.EncodeToString(fingerprint[:])
}
