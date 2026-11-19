package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPTClientLocalTimeReplayedHTTPHistory(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
					Credentials: map[string]any{"api_key": "synthetic", "base_url": "https://api.openai.com/v1"},
					Extra:       map[string]any{openAIRequestTimezoneExtraKey: "America/Los_Angeles", "openai_responses_supported": true}}
				upstream := &httpUpstreamRecorder{}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, tiboRouteDisabled: true,
					requestTimezoneResolver: NewOpenAIRequestTimezoneResolver(nil, "")}
				env := func(date, clock, zone string) string {
					return "<environment_context><current_date>" + date + "</current_date><current_time>" + clock + "</current_time><timezone>" + zone + "</timezone></environment_context>"
				}
				var messages []map[string]string
				forward := func() gjson.Result {
					field := "input"
					if path == "/v1/chat/completions" {
						field = "messages"
					}
					body, err := json.Marshal(map[string]any{"model": "gpt-5.4-mini", field: messages, "stream": true})
					require.NoError(t, err)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
					c.Set("api_key", &APIKey{ID: 32, UserID: 45})
					upstream.resp = openAICompatSSECompletedResponse("resp_history", "gpt-5.4-mini")
					if path == "/v1/chat/completions" {
						_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
					} else {
						_, err = svc.Forward(context.Background(), c, account, body)
					}
					require.NoError(t, err)
					require.Equal(t, "/v1/responses", upstream.lastReq.URL.Path)
					return gjson.GetBytes(upstream.lastBody, "input")
				}

				// Within the test clock, both the gateway and client cross a local
				// midnight on this sequence; each call has a fresh HTTP context.
				la, err := time.LoadLocation("America/Los_Angeles")
				require.NoError(t, err)
				now := time.Now().In(la)
				midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, la)
				time.Sleep(midnight.Sub(now) - 90*time.Second)
				messages = append(messages, map[string]string{"role": "user", "content": env("2026-07-02", "06:58:30", "UTC")})
				first := forward().Array()
				require.Len(t, first, 1)
				require.Contains(t, first[0].Raw, "23:58:30")
				require.Contains(t, first[0].Raw, "2026-07-01")

				for _, tc := range []struct{ source, want string }{{"06:59:30", "23:59:30"}, {"07:00:30", "00:00:30"}} {
					time.Sleep(time.Minute)
					messages = append(messages, map[string]string{"role": "user", "content": env("2026-07-02", tc.source, "UTC")})
					next := forward().Array()
					require.Len(t, next, len(first)+1)
					for i, previous := range first {
						require.Equal(t, previous.Raw, next[i].Raw, "already forwarded input item %d", i)
					}
					require.Contains(t, next[len(first)].Raw, tc.want)
					first = next
				}
			})
		})
	}
}

func TestGPTClientLocalTimePreservesUnrelatedJSONBytes(t *testing.T) {
	projection := &OpenAIGatewayService{}
	account := &Account{Platform: PlatformOpenAI}
	prefix := " \t" + `{"model":"gpt-5.4-mini","metadata":{"escaped":"\u03b5 < > &"},"unrelated":{"current_time":"01:02:03"},"input":[` +
		`{"type":"function_call_output","output":"<environment_context><current_date>2026-07-02</current_date><current_time>07:00:00</current_time><timezone>UTC</timezone></environment_context>"},` +
		`{"role":"user","content":"Explain this: <environment_context><current_date>2026-07-02</current_date><current_time>07:00:00</current_time><timezone>UTC</timezone></environment_context>"},` +
		`{"role":"assistant","content":"<environment_context><current_date>2026-07-02</current_date><current_time>07:00:00</current_time><timezone>UTC</timezone></environment_context>"},`
	suffix := `],"tools":[{"name":"foo","parameters":{"default":{"current_date":"2026-07-02","current_time":"01:02:03","timezone":"UTC"}}}],"number":9007199254740993}` + "\n"
	field := `{"role":"user","content":[{"type":"input_text","text":"\u003cenvironment_context\u003e\n<cwd>日本語 &amp; ü</cwd><current_date>2026-07-02</current_date><current_time>06:59:59</current_time><timezone>UTC</timezone></environment_context>"}]}`
	out := projection.rewriteOpenAIClientLocalTimeIfEnabled(account, []byte(prefix+field+suffix))
	require.True(t, bytes.HasPrefix(out, []byte(prefix)))
	require.True(t, bytes.HasSuffix(out, []byte(suffix)))
	text := gjson.GetBytes(out, "input.3.content.0.text").String()
	require.Contains(t, text, "<cwd>日本語 &amp; ü</cwd>")
	require.Contains(t, text, "<current_time>23:59:59</current_time>")
	require.Contains(t, text, "<current_date>2026-07-01</current_date>")
	require.JSONEq(t, string(out), string(projection.rewriteOpenAIClientLocalTimeIfEnabled(account, out)))
}

func TestGPTClientLocalTimeRetainsTimezonePolicyAndTrustedClient(t *testing.T) {
	svc := &OpenAIGatewayService{requestTimezoneResolver: NewOpenAIRequestTimezoneResolver(nil, "Asia/Tokyo")}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	body := []byte(`{"model":"gpt-6.2-sol","metadata":{"current_date":"2026-01-01","current_time":"23:15:11","timezone":"UTC"},"web_search_options":{"user_location":{"approximate":{"timezone":"UTC+8"}}},"input":"hello"}`)
	tokyo := svc.rewriteOpenAIClientLocalTimeIfEnabled(account, body)
	require.Equal(t, "2026-01-02", gjson.GetBytes(tokyo, "metadata.current_date").Str)
	require.Equal(t, "08:15:11", gjson.GetBytes(tokyo, "metadata.current_time").Str)
	require.Equal(t, "Asia/Tokyo", gjson.GetBytes(tokyo, "web_search_options.user_location.approximate.timezone").Str)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "codex_exec/0.166.0")
	c.Request.Header.Set("originator", "codex_exec")
	require.Equal(t, body, svc.rewriteOpenAIClientLocalTimeForRequest(c, account, body))
	svc.cfg = &config.Config{Gateway: config.GatewayConfig{DisableOpenAIClientLocalTimeRewrite: true}}
	require.Equal(t, body, svc.rewriteOpenAIClientLocalTimeIfEnabled(account, body))
}

func TestGPTClientLocalTimeBoundsSourceLookupsAndDepth(t *testing.T) {
	p := openAIClientClockProjection{targetName: "UTC"}
	for i := 0; i <= openAIClientClockMaxZones; i++ {
		for range 2 {
			_, ok := p.convert("2026-07-02", "06:59:30", fmt.Sprintf("Missing/Zone%d", i))
			require.False(t, ok)
		}
	}
	require.Len(t, p.zones, openAIClientClockMaxZones)
	clock := `{"current_date":"2026-01-01","current_time":"23:15:11","timezone":"UTC"}`
	deep := []byte(`{"model":"gpt-5.4-mini","input":` + strings.Repeat("[", 64) + clock + strings.Repeat("]", 64) + "}")
	require.Equal(t, deep, rewriteGPTClientLocalTimes(deep, "Asia/Tokyo"))
}

func TestGPTClientLocalTimeUsesActualForwardModel(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{"public-chat": "gpt-5.4-mini", "gpt-6.1-sol": "other-model"}}}
	svc := &OpenAIGatewayService{}
	body := func(model string) []byte {
		return []byte(`{"model":"` + model + `","input":"<environment_context><current_time>legacy</current_time><timezone>UTC</timezone></environment_context>"}`)
	}
	require.Equal(t, body("public-chat"), svc.rewriteOpenAIClientLocalTimeIfEnabled(account, body("public-chat")))
	other := svc.rewriteOpenAIClientLocalTimeIfEnabled(account, body("gpt-6.1-sol"))
	require.Contains(t, string(other), "America/Los_Angeles", "non-GPT mapped targets retain the prior timezone policy")
}

func BenchmarkGPTClientLocalTimeLargeRequest(b *testing.B) {
	plain := []byte(`{"model":"gpt-6.2-sol","input":[{"role":"user","content":"` + strings.Repeat("x", 4<<20) + `"}]}`)
	deep := []byte(`{"model":"gpt-6.2-sol","input":` + strings.Repeat("[", 160) + `{"current_date":"2026-01-01","current_time":"00:00:00","timezone":"UTC"}` + strings.Repeat("]", 160) + `}`)
	svc := &OpenAIGatewayService{}
	account := &Account{Platform: PlatformOpenAI}
	for _, tc := range []struct {
		name string
		body []byte
	}{{"ordinary", plain}, {"deep", deep}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.body)))
			for b.Loop() {
				svc.rewriteOpenAIClientLocalTimeForModel(nil, account, tc.body, "gpt-6.2-sol")
			}
		})
	}
}

func TestGPTClientLocalTimeSourceAndScope(t *testing.T) {
	service := &OpenAIGatewayService{}
	account := &Account{Platform: PlatformOpenAI}
	for _, tc := range []struct{ name, source, want string }{
		{"summer", `{"current_date":"2026-07-02","current_time":"06:59:30","timezone":"UTC"}`,
			`{"current_date":"2026-07-01","current_time":"23:59:30","timezone":"America/Los_Angeles"}`},
		{"winter", `{"timezone":"Asia/Shanghai","current_time":"15:00:01","current_date":"2026-01-02"}`,
			`{"timezone":"America/Los_Angeles","current_time":"23:00:01","current_date":"2026-01-01"}`},
		{"missing_date", `{"current_time":"00:00:10","timezone":"UTC"}`, ""},
		{"missing_time", `{"current_date":"2026-11-01","timezone":"UTC"}`, ""},
		{"missing_zone", `{"current_date":"2026-11-01","current_time":"01:30:00"}`, ""},
		{"ambiguous_fall_back", `{"current_date":"2026-11-01","current_time":"01:30:00","timezone":"America/New_York"}`, ""},
		{"nonexistent_spring_forward", `{"current_date":"2026-03-08","current_time":"02:15:00","timezone":"America/New_York"}`, ""},
		{"invalid_date", `{"current_date":"2026-02-30","current_time":"01:30:00","timezone":"UTC"}`, ""},
		{"invalid_clock", `{"current_date":"2026-04-12","current_time":"25:25:25","timezone":"UTC"}`, ""},
		{"host_local_zone", `{"current_date":"2026-04-12","current_time":"01:30:00","timezone":"Local"}`, ""},
		{"nested_fields", `{"current_date":"2026-11-01","metadata":{"current_time":"01:30:00","timezone":"UTC"}}`, ""},
		{"duplicate_field", `{"current_date":"2026-11-01","current_time":"01:30:00","timezone":"UTC","timezone":"GMT"}`, ""},
		{"quoted_json", `{"content":"example: {\"current_date\":\"2026-07-02\",\"current_time\":\"07:00:00\",\"timezone\":\"UTC\"}"}`, ""},
		{"xml_aliases", `{"text":"<environment_context><local_date>2026-07-02</local_date><time_zone>UTC</time_zone><current_time> 06:59:30 </current_time></environment_context>"}`,
			`{"text":"<environment_context><local_date>2026-07-01</local_date><time_zone>America/Los_Angeles</time_zone><current_time> 23:59:30 </current_time></environment_context>"}`},
		{"dated_text", `{"text":"<environment_context><current_date>2026-07-02</current_date><timezone>UTC</timezone><current_time>Friday, July 3 at 06:59 GMT</current_time></environment_context>"}`, ""},
		{"sibling_clocks", `{"text":"<environment_context><current_date>2026-07-02</current_date></environment_context><environment_context><current_time>06:59:30</current_time><timezone>UTC</timezone></environment_context>"}`, ""},
		{"duplicate_xml_zone", `{"text":"<environment_context><current_date>2026-07-02</current_date><timezone>UTC</timezone><time_zone>Asia/Tokyo</time_zone><current_time>06:59:30</current_time></environment_context>"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.4-mini","input":[` + tc.source + `]}`)
			out := service.rewriteOpenAIClientLocalTimeIfEnabled(account, body)
			if tc.want == "" {
				require.Equal(t, body, out)
			} else {
				require.JSONEq(t, tc.want, gjson.GetBytes(out, "input.0").Raw)
				require.Equal(t, out, service.rewriteOpenAIClientLocalTimeIfEnabled(account, out))
			}
		})
	}
}
