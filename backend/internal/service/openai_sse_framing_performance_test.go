package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPerformanceOptimizationSSEFraming(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", false},
		{"empty lines", "\n\r\n\r\r\n", false},
		{"data without newline", "data: {}", true},
		{"event without newline", "event: response.completed", true},
		{"empty data", "data:", true},
		{"empty event", "event:", true},
		{"LF", "data: {}\n\n", true},
		{"CRLF", "data: {}\r\n\r\n", true},
		{"repeated trailing CR", "event: response.completed\r\r\n", true},
		{"comments and blank lines", ": comment data: ignored\r\n\r\ndata: {}", true},
		{"only comments", ": data: {}\n: event: response.completed\n", false},
		{"data after LF", "not a field\ndata: {}", true},
		{"event after CRLF", "not a field\r\nevent: response.completed", true},
		{"mixed line endings", ": keepalive\r\n\nevent: response.completed\r\ndata: {}\n", true},
		{"leading CR", "\rdata: {}", false},
		{"bare CR is not a separator", ": keepalive\rdata: {}", false},
		{"bare CR within first data line", "data: {}\revent: response.completed", true},
		{"space indentation", " data: {}\n event: response.completed", false},
		{"tab indentation", "\tdata: {}\n\tevent: response.completed", false},
		{"uppercase", "Data: {}\nEvent: response.completed", false},
		{"space before colon", "data : {}\nevent : response.completed", false},
		{"missing colon", "data\nevent", false},
		{"partial names", "dat:\neven:", false},
		{"similar names", "metadata: {}\nevents: done", false},
		{"unicode line separator", "text\u2028data: {}", false},
		{"UTF8 BOM", "\ufeffdata: {}", false},
		{"NUL before field", "\x00data: {}", false},
		{"compact JSON text", `{"text":"data: {} event: response.completed"}`, false},
		{"escaped newline in JSON", `{"text":"\ndata: {}\r\nevent: response.completed"}`, false},
		{"pretty JSON", "{\n  \"data:\": {},\n  \"event:\": \"done\"\n}\n", false},
		{"quoted physical lines", "\"data: {}\"\n\"event: done\"", false},
		{"malformed JSON with physical data line", "{\"text\":\"broken\ndata: value", true},
		{"last line without newline", ": keepalive\n\ndata: [DONE]", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(test.body)
			require.Equal(t, test.want, bodyHasSSEFraming(body))
			for end := 0; end <= len(body); end++ {
				require.Equalf(t, referencePerformanceSSEFraming(body[:end]), bodyHasSSEFraming(body[:end]), "truncated at byte %d", end)
			}
		})
	}
}

// This is the pre-optimization detector, retained only as an equivalence oracle.
func referencePerformanceSSEFraming(body []byte) bool {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:")) {
			return true
		}
	}
	return false
}

func TestPerformanceOptimizationSSENonStreamingRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const responseJSON = `{"id":"resp_perf_sse","object":"response","created_at":1,"model":"gpt-5.4","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":3}}}`
	for _, passthrough := range []bool{false, true} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			for _, contentType := range []string{"", "application/json", "text/event-stream; charset=utf-8"} {
				for _, newline := range []string{"\n", "\r\n"} {
					t.Run(fmt.Sprintf("passthrough=%v/%s/header=%q/newline=%q", passthrough, accountType, contentType, newline), func(t *testing.T) {
						body := ": keepalive" + newline + newline + "event: response.completed" + newline +
							`data: {"type":"response.completed","response":` + responseJSON + "}" + newline + newline + "data: [DONE]"
						rec, c, usage := runPerformanceNonStreamingResponse(t, passthrough, accountType, contentType, body)
						require.Equal(t, http.StatusCreated, rec.Code)
						require.Equal(t, 11, usage.InputTokens)
						require.Equal(t, 7, usage.OutputTokens)
						require.Equal(t, 3, usage.CacheReadInputTokens)
						require.Equal(t, "gpt-5.4", observedUpstreamResponseModel(c))
						// API-key passthrough deliberately preserves the raw SSE if
						// Content-Type does not identify it; legacy retries parsing it.
						if passthrough && accountType == AccountTypeAPIKey && !strings.Contains(contentType, "text/event-stream") {
							require.Equal(t, body, rec.Body.String())
							require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
						} else {
							require.JSONEq(t, responseJSON, rec.Body.String())
							require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
						}
					})
				}
			}
		}
	}
}

func TestPerformanceOptimizationSSENonStreamingJSONText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const responseJSON = `{"id":"resp_perf_json","object":"response","created_at":1,"model":"gpt-5.4","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"data: literal\nevent: text\r\ndata: after escaped newline"}]}],"usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":3}}}`
	var pretty bytes.Buffer
	require.NoError(t, json.Indent(&pretty, []byte(responseJSON), "", "  "))
	for _, passthrough := range []bool{false, true} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			for _, contentType := range []string{"", "application/json"} {
				for _, format := range []struct {
					name string
					body string
				}{
					{"compact", responseJSON},
					{"LF", pretty.String()},
					{"CRLF", strings.ReplaceAll(pretty.String(), "\n", "\r\n")},
				} {
					t.Run(fmt.Sprintf("passthrough=%v/%s/header=%q/%s", passthrough, accountType, contentType, format.name), func(t *testing.T) {
						rec, c, usage := runPerformanceNonStreamingResponse(t, passthrough, accountType, contentType, format.body)
						require.Equal(t, http.StatusCreated, rec.Code)
						require.Equal(t, 11, usage.InputTokens)
						require.Equal(t, 7, usage.OutputTokens)
						require.Equal(t, 3, usage.CacheReadInputTokens)
						require.JSONEq(t, responseJSON, rec.Body.String())
						require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
						require.Equal(t, "gpt-5.4", observedUpstreamResponseModel(c))
						require.Equal(t, "data: literal\nevent: text\r\ndata: after escaped newline", gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
					})
				}
			}
		}
	}
}

func runPerformanceNonStreamingResponse(t *testing.T, passthrough bool, accountType, contentType, body string) (*httptest.ResponseRecorder, *gin.Context, *OpenAIUsage) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusCreated,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	if contentType != "" {
		resp.Header.Set("Content-Type", contentType)
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 1, Type: accountType, Platform: PlatformOpenAI}
	if passthrough {
		result, err := svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "gpt-5.4", "gpt-5.4")
		require.NoError(t, err)
		require.NotNil(t, result)
		return rec, c, result.OpenAIUsage
	}
	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, "gpt-5.4", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	return rec, c, result.OpenAIUsage
}
