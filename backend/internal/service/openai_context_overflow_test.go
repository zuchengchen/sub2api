package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func firstSSEData(t *testing.T, body string) string {
	t.Helper()
	parts := strings.SplitN(body, "data: ", 2)
	require.Len(t, parts, 2, body)
	return strings.TrimSpace(strings.SplitN(parts[1], "\n", 2)[0])
}

// An upstream overflow reported over HTTP (any status) must reach a streaming
// Codex client as the terminal response.failed with code
// context_length_exceeded; an HTTP 400 JSON body never triggers compaction.
func TestOpenAIHandleErrorResponse_ContextWindowStreamWritesFailedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, status := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusBadGateway} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		MarkOpenAIResponsesClientStream(c, true)

		respBody := []byte(`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again.","type":"invalid_request_error","code":"context_length_exceeded"}}`)
		resp := &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(respBody)), Header: http.Header{}}
		svc := &OpenAIGatewayService{}
		_, err := svc.handleErrorResponse(context.Background(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, nil)
		require.Error(t, err)
		require.Equal(t, http.StatusOK, rec.Code, "status %d", status)
		require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))
		data := firstSSEData(t, rec.Body.String())
		require.Equal(t, "context_length_exceeded", gjson.Get(data, "response.error.code").String())
		require.True(t, IsResponseCommitted(c))
	}
}

func TestIsOpenAIContextWindowErrorMatchesAnthropicWording(t *testing.T) {
	require.True(t, isOpenAIContextWindowError("prompt is too long: 210000 tokens > 200000 maximum", nil))
	require.False(t, isOpenAIContextWindowError("Request payload is too large", nil))
}

// A pre-output response.failed whose code is null still gets
// context_length_exceeded so Codex compacts; the upstream type is kept.
func TestSanitizeResponseFailedFillsContextLengthCodeBeforeOutput(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"type":"upstream_error","message":"Your input exceeds the context window of this model.","code":null}}}`)
	out, changed := sanitizeOpenAIResponseFailedEventForClient(payload, "response.failed", false)
	require.True(t, changed)
	require.Equal(t, "context_length_exceeded", gjson.GetBytes(out, "response.error.code").String())
	require.Equal(t, "upstream_error", gjson.GetBytes(out, "response.error.type").String())
}

func TestCompatErrorWritersMapContextOverflow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	msg := "Your input exceeds the context window of this model."

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	writeAnthropicCompatError(c, http.StatusBadGateway, "api_error", msg)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.True(t, strings.HasPrefix(gjson.Get(rec.Body.String(), "error.message").String(), "prompt is too long"))

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	writeChatCompletionsCompatError(c, http.StatusBadGateway, "upstream_error", msg)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "context_length_exceeded", gjson.Get(rec.Body.String(), "error.code").String())

	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	writeAnthropicCompatError(c, http.StatusBadGateway, "api_error", "Upstream service temporarily unavailable")
	require.Equal(t, http.StatusBadGateway, rec.Code, "non-overflow errors keep their mapping")
}

// Inline PDFs and other base64 files are billed by decoded content; their
// base64 length must not trip the synthesis budget.
func TestCodexSynthesisEstimateCapsInlineFiles(t *testing.T) {
	pdf := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("%PDF-1.7 stream"), 300_000))
	responsesFile := map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "input_file", "filename": "a.pdf", "file_data": "data:application/pdf;base64," + pdf},
	}}
	anthropicDoc := map[string]any{"type": "message", "role": "user", "content": []any{
		map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": pdf}},
	}}
	got := codexSynthesisEstimatedTokens([]any{responsesFile, anthropicDoc})
	require.Less(t, got, 4000, "naive len/4 would be ~%d", len(pdf)/2)

	text := map[string]any{"type": "message", "role": "user", "content": strings.Repeat("x", 4000)}
	require.Greater(t, codexSynthesisEstimatedTokens([]any{text}), 999, "plain text keeps len/4")
}

// An official client whose input starts with its own developer prompt must not
// get a second, default top-level instructions; one without it still does.
func TestOfficialClientDefaultInstructionsOnlyWithoutOwnPrompt(t *testing.T) {
	require.True(t, codexInputLeadsWithDeveloperMessage([]byte(`{"input":[{"type":"message","role":"developer","content":"base"}]}`)))
	require.False(t, codexInputLeadsWithDeveloperMessage([]byte(`{"input":[{"type":"text","text":"hi"}]}`)))
	require.False(t, codexInputLeadsWithDeveloperMessage([]byte(`{"input":[{"type":"additional_tools","role":"developer","tools":[]}]}`)))
}

// In-flight balance reservation: Codex bodies carry reasoning
// encrypted_content and screenshots that cost far fewer tokens than their
// base64 size. Plain bodies are untouched.
func TestInflightEffectiveBodyBytes(t *testing.T) {
	plain := []byte(`{"model":"gpt-6-astra","input":[{"type":"message","role":"user","content":"` + strings.Repeat("a", 40_000) + `"}]}`)
	require.Equal(t, len(plain), InflightEffectiveBodyBytes(plain))

	blob := strings.Repeat("Z", 300_000)
	codex := []byte(`{"model":"gpt-6-astra","input":[` +
		`{"type":"reasoning","summary":[],"encrypted_content":"` + blob + `"},` +
		`{"type":"message","role":"user","content":"` + strings.Repeat("a", 40_000) + `"}]}`)
	got := InflightEffectiveBodyBytes(codex)
	require.Less(t, got, 45_000, "encrypted_content must not be reserved as ~75k input tokens")
	require.Greater(t, got, 40_000)

	pdf := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("%PDF"), 200_000))
	withFile := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_data":"data:application/pdf;base64,` + pdf + `"}]}]}`)
	require.Less(t, InflightEffectiveBodyBytes(withFile), 10_000)

	require.Equal(t, 12, InflightEffectiveBodyBytes([]byte(`not json ;base64,`)[:12]))
}
