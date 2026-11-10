package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func benchmarkCodexOAuthAccount(mode string) *Account {
	claims := `{"exp":4102444800,"https://api.openai.com/auth":{"chatgpt_account_id":"bench-workspace","chatgpt_user_id":"bench-user"},"aud":"bench","jti":"snapshot"}`
	return &Account{ID: 71001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Concurrency: 8,
		Credentials: map[string]any{"access_token": "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature", "refresh_token": "synthetic-refresh", "chatgpt_account_id": "bench-workspace", "chatgpt_user_id": "bench-user"},
		Extra:       map[string]any{"codex_fingerprint_mode": mode, "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111"}}
}

// Include OAuth modes: the existing pool benchmark uses an API-key account
// and cannot measure the new OAuth continuity check.
func BenchmarkCodexOAuthPoolAcquire(b *testing.B) {
	for _, mode := range []string{"off", "device", "full"} {
		b.Run(mode, func(b *testing.B) {
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 8
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 8
			cfg.Gateway.OpenAIWS.QueueLimitPerConn = 256
			cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 1
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&openAIWSCountingDialer{})
			defer pool.Close()
			account := benchmarkCodexOAuthAccount(mode)
			req := openAIWSAcquireRequest{Account: account, WSURL: "wss://example.invalid/responses", Headers: http.Header{
				"Session-Id": {"019a699f-0000-7000-8000-000000000001"}, "Thread-Id": {"019a699f-0000-7000-8000-000000000001"},
				"X-Codex-Installation-Id": {"11111111-1111-4111-8111-111111111111"}, "X-Codex-Window-Id": {"019a699f-0000-7000-8000-000000000001:0"},
			}}
			ctx := context.Background()
			lease, err := pool.Acquire(ctx, req)
			if err != nil {
				b.Fatal(err)
			}
			lease.Release()
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					lease, err := pool.Acquire(ctx, req)
					if err != nil {
						b.Fatal(err)
					}
					lease.Release()
				}
			})
		})
	}
}

type benchmarkCodexOAuthTokenCache struct {
	TokenCache
	token string
}

func (c benchmarkCodexOAuthTokenCache) GetAccessToken(context.Context, string) (string, error) {
	return c.token, nil
}

func BenchmarkCodexOAuthCachedToken(b *testing.B) {
	for _, rotated := range []bool{false, true} {
		b.Run(fmt.Sprintf("rotated=%t", rotated), func(b *testing.B) {
			account := benchmarkCodexOAuthAccount("full")
			token := account.GetOpenAIAccessToken()
			if rotated {
				claims := `{"exp":4102444900,"https://api.openai.com/auth":{"chatgpt_account_id":"bench-workspace","chatgpt_user_id":"bench-user"},"aud":"bench","jti":"rotated"}`
				token = "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
			}
			provider := NewOpenAITokenProvider(nil, benchmarkCodexOAuthTokenCache{token: token}, nil)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := provider.GetAccessToken(context.Background(), account)
				if err != nil || result != token {
					b.Fatalf("cache hit failed: %v", err)
				}
				benchmarkOpenAIWSStringSink = result
			}
		})
	}
}

func BenchmarkCodexOAuthPromptCacheFilter(b *testing.B) {
	for _, size := range []int{1024, 64 * 1024} {
		for _, options := range []string{"absent", "native_ttl", "platform_breakpoint"} {
			for _, escaped := range []bool{false, true} {
				b.Run(fmt.Sprintf("bytes=%d/%s/escaped=%t", size, options, escaped), func(b *testing.B) {
					fields := `"prompt_cache_key":"session",`
					part := ""
					if options == "native_ttl" {
						fields += `"prompt_cache_options":{"ttl":"12h"},`
					} else if options == "platform_breakpoint" {
						fields += `"prompt_cache_options":{"mode":"explicit"},`
						part = `,"prompt_cache_breakpoint":{}`
					}
					content := strings.Repeat("x", size)
					if escaped {
						content = strings.Repeat(`document line\n`, size/15)
					}
					body := []byte(`{"model":"gpt-6-astra",` + fields + `"input":[{"role":"user","content":[{"type":"input_text","text":"` + content + `"` + part + `}]}]}`)
					account := benchmarkCodexOAuthAccount("full")
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						result, _, err := filterOpenAIResponsesPromptCacheConfiguration(account, "gpt-6-astra", body)
						if err != nil {
							b.Fatal(err)
						}
						benchmarkOpenAIWSBytesSink = result
					}
				})
			}
		}
	}
}
