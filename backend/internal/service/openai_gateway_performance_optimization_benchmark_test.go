package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

var (
	benchmarkPerformanceOptimizationSeed string
	benchmarkPerformanceOptimizationSSE  bool
)

// Keep this benchmark self-contained: the same source runs unchanged on the
// pre-optimization baseline and the candidate, without candidate-only helpers.
func BenchmarkPerformanceOptimizationGrok(b *testing.B) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: 301, Group: &Group{Platform: PlatformGrok}})

	for _, sizeMiB := range []int{4, 16} {
		for _, field := range []string{"messages", "input"} {
			b.Run(fmt.Sprintf("%s/%dMiB", field, sizeMiB), func(b *testing.B) {
				history := strings.Repeat("h", sizeMiB<<20)
				makeBody := func(prefix bool, history string) []byte {
					system := ""
					if prefix {
						system = `{"role":"system","content":"Be concise."},`
					}
					return []byte(`{"model":"grok-4","` + field + `":[` + system +
						`{"role":"user","content":"First question"},` +
						`{"role":"assistant","content":"` + history + `"},` +
						`{"role":"user","content":"Follow-up"}]}`)
				}
				stableBody, anchorBody := makeBody(true, history), makeBody(false, history)
				stableSeed := deriveOpenAIStablePrefixSessionSeed(makeBody(true, ""))
				anchorSeed := deriveOpenAIAnchoredContentSessionSeed(makeBody(false, ""))
				stableIdentity := generateSessionUUID("grok-prompt-cache:v1:301:grok-4:" + stableSeed)
				anchorIdentity := generateSessionUUID("grok-prompt-cache:v1:301:grok-4:" + anchorSeed)
				for _, test := range []struct {
					name string
					body []byte
					want string
					fn   func([]byte) string
				}{
					{"StablePrefix", stableBody, stableSeed, deriveOpenAIStablePrefixSessionSeed},
					{"AnchoredContent", anchorBody, anchorSeed, deriveOpenAIAnchoredContentSessionSeed},
					{"IdentityStablePrefix", stableBody, stableIdentity, func(body []byte) string {
						return resolveGrokCacheIdentity(c, body, "", "grok-4")
					}},
					{"IdentityAnchoredFallback", anchorBody, anchorIdentity, func(body []byte) string {
						return resolveGrokCacheIdentity(c, body, "", "grok-4")
					}},
				} {
					b.Run(test.name, func(b *testing.B) {
						if got := test.fn(test.body); got == "" || got != test.want {
							b.Fatalf("history changed seed/identity: got %q, want %q", got, test.want)
						}
						b.SetBytes(int64(len(test.body)))
						b.ReportAllocs()
						b.ResetTimer()
						for range b.N {
							benchmarkPerformanceOptimizationSeed = test.fn(test.body)
						}
					})
				}
			})
		}
	}
}

func BenchmarkPerformanceOptimizationSSEFraming(b *testing.B) {
	for _, test := range []struct {
		name string
		body []byte
		want bool
	}{
		{"CompactJSON4MiB", []byte(`{"output":"` + strings.Repeat("x", 4<<20) + `data: literal event: text"}`), false},
		{"MultilineJSONLF", []byte("{\n\"items\":[\n" + strings.Repeat("{\"text\":\"data: event:\"},\n", 50000) + "null\n]}\n"), false},
		{"MultilineJSONCRLF", []byte("{\r\n\"items\":[\r\n" + strings.Repeat("{\"text\":\"data: event:\"},\r\n", 50000) + "null\r\n]}\r\n"), false},
		{"MultiFrameSSELF", []byte(strings.Repeat("event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\n", 20000)), true},
		{"MultiFrameSSECRLF", []byte(strings.Repeat("event: response.output_text.delta\r\ndata: {\"delta\":\"hello\"}\r\n\r\n", 20000)), true},
		{"LateFrame", []byte(strings.Repeat(": keepalive\n\n", 50000) + "data: [DONE]"), true},
	} {
		b.Run(test.name, func(b *testing.B) {
			if got := bodyHasSSEFraming(test.body); got != test.want {
				b.Fatalf("framing: got %v, want %v", got, test.want)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				benchmarkPerformanceOptimizationSSE = bodyHasSSEFraming(test.body)
			}
		})
	}
}
