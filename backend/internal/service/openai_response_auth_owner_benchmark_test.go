package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// This file uses only production APIs and fixtures present at b86090b548a2.
// Overlay the identical file on both revisions for an interpretable percentage.
// Each operation executes a complete seeded/full HTTP passthrough request and
// consumes the synthetic upstream SSE response. Response IDs are fixed to keep
// repeated benchmarking from measuring unbounded synthetic state growth.
type responseOwnerBenchmarkUpstream struct{ expectedPrevious string }

func (u *responseOwnerBenchmarkUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	_ = req.Body.Close()
	if got := gjson.GetBytes(body, "previous_response_id").String(); got != u.expectedPrevious {
		return nil, fmt.Errorf("unexpected synthetic previous response: %q", got)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_benchmark_output\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n"))}, nil
}
func (u *responseOwnerBenchmarkUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func BenchmarkOpenAIResponseOwnerHTTPPassthrough(b *testing.B) {
	gin.SetMode(gin.TestMode)
	logger.InitBootstrap()
	previousLevel := logger.CurrentLevel()
	if err := logger.SetLevel("error"); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = logger.SetLevel(previousLevel) })
	for _, withPrevious := range []bool{false, true} {
		name := "EmptyID"
		if withPrevious {
			name = "KnownSameOwner"
		}
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			upstream := &responseOwnerBenchmarkUpstream{}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, tiboRouteDisabled: true, openaiWSStateStore: NewOpenAIWSStateStore(nil)}
			account := &Account{ID: 821, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"access_token": "synthetic-benchmark-access", "chatgpt_account_id": "workspace", "chatgpt_user_id": "owner"}, Extra: astraHealthyTestExtra(map[string]any{"openai_passthrough": true, "openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff, "codex_fingerprint_mode": "full", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111"})}
			body := []byte(`{"model":"gpt-5.2","stream":false,"instructions":"synthetic benchmark","input":[{"role":"user","content":"continue the synthetic conversation"}]}`)
			if withPrevious {
				upstream.expectedPrevious = "resp_benchmark_origin"
				body = []byte(`{"model":"gpt-5.2","stream":false,"instructions":"synthetic benchmark","previous_response_id":"resp_benchmark_origin","input":[{"role":"user","content":"continue the synthetic conversation"}]}`)
			}
			newRequest := func() *gin.Context {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("User-Agent", "codex_cli_rs/0.168.0")
				c.Request.Header.Set("Session-Id", "019aa001-1000-7000-8000-000000000001")
				c.Set("api_key", &APIKey{ID: 201, UserID: 101})
				SetOpenAIHTTPResponseOwner(c, 101, 201)
				return c
			}
			if withPrevious {
				svc.bindHTTPResponseAccount(ctx, newRequest(), account, upstream.expectedPrevious)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, err := svc.Forward(ctx, newRequest(), account, body)
				if err != nil || result == nil {
					b.Fatalf("synthetic production forward failed: %v", err)
				}
			}
		})
	}
}
