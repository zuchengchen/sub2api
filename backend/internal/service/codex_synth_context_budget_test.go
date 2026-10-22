package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// gpt-6.1-sol is advertised on /models with a 500k window (872k max). The
// synthesis budget must use the same numbers, not the pinned 272k catalog
// value minus a 25% reserve (~204k), or clients never compact before the
// gateway rejects them.
func TestCodexSynthesisContextLimitsMatchAdvertisedWindow(t *testing.T) {
	account := newCodexSynthTestAccount(7701, AccountTypeOAuth)
	descriptor, ok := openai.CodexCatalogModelForSlug("gpt-6.1-sol")
	require.True(t, ok)

	limits := codexSynthesisContextLimitsFor(account, "gpt-6.1-sol", descriptor, ok)
	advertised := newConfiguredCodexModelDescriptor("gpt-6.1-sol")
	require.Equal(t, int(advertised.ContextWindow), limits.Advertised)
	require.Equal(t, 500_000, limits.Advertised)
	require.Equal(t, 872_000, limits.Hard)

	// Synced account metadata wins over local defaults.
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Source: "test",
		Models: map[string]UpstreamModelMetadata{
			"gpt-6.1-sol": {ID: "gpt-6.1-sol", ContextWindow: 400_000, MaxContextWindow: 600_000},
		},
	})
	limits = codexSynthesisContextLimitsFor(account, "gpt-6.1-sol", descriptor, ok)
	require.Equal(t, codexSynthContextLimits{Advertised: 400_000, Hard: 600_000}, limits)

	// Unknown models still get a usable fallback.
	limits = codexSynthesisContextLimitsFor(nil, "custom-model-x", descriptor, false)
	require.Equal(t, configuredCodexFallbackContext, limits.Advertised)
	require.GreaterOrEqual(t, limits.Hard, limits.Advertised)
}

func TestCodexSynthesisContextLimitsAllowLongConversation(t *testing.T) {
	descriptor, ok := openai.CodexCatalogModelForSlug("gpt-6.1-sol")
	require.True(t, ok)
	limits := codexSynthesisContextLimitsFor(nil, "gpt-6.1-sol", descriptor, ok)
	// ~300k estimated tokens: rejected before (budget ~204k), accepted now.
	user := map[string]any{"type": "message", "role": "user", "content": strings.Repeat("a", 1_200_000)}
	_, truncated, err := codexSynthesisTrimToBudget([]any{user}, limits, nil)
	require.NoError(t, err)
	require.False(t, truncated)
}

func TestCodexSynthesisEstimateIgnoresEncryptedContent(t *testing.T) {
	blob := strings.Repeat("Z", 400_000)
	reasoning := map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": blob}
	compaction := json.RawMessage(`{"type":"compaction","encrypted_content":"` + blob + `"}`)
	got := codexSynthesisEstimatedTokens([]any{reasoning, compaction})
	require.Less(t, got, 100, "opaque replay blobs must not be charged as ~200k text tokens")
}

// Official Codex clients configured with a custom model_provider strip
// internal_chat_message_metadata_passthrough (is_openai()==false), so the
// base-instructions marker is missing. They must still be treated as native
// and not get a second base prompt or the synthesis budget.
func TestCodexSynthesisOfficialClientWithoutMarkerIsNative(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
	account := newCodexSynthTestAccount(7702, AccountTypeOAuth)
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"client base prompt"}]},` +
		`{"type":"message","role":"user","content":"hello"}],` +
		`"tools":[{"type":"function","name":"exec_command","parameters":{"type":"object"}}]}`)

	upstream := runCodexSynthForward(t, svc, account, body, map[string]string{
		"User-Agent": "Codex Desktop/0.162.0-alpha.17.2 (Mac OS 13.7.8; arm64) unknown (Codex Desktop; 26.1007.30912)",
		"originator": "Codex Desktop",
	})
	sent := upstream.lastBody
	require.Equal(t, "client base prompt", gjson.GetBytes(sent, "input.0.content.0.text").String())
	require.NotContains(t, string(sent), "<skills_instructions>")
	require.NotContains(t, string(sent), "<permissions instructions>")
}

func TestCodexSynthesisContextOverflowReturnsContextLengthExceeded(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
	svc.tiboRouteDisabled = true
	svc.httpUpstream = &httpUpstreamRecorder{}
	account := newCodexSynthTestAccount(7703, AccountTypeOAuth)
	// gpt-5.5 hard limit is 272k; ~2.5M bytes is ~625k estimated tokens.
	huge := strings.Repeat("a", 2_500_000)
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message","role":"user","content":"` + huge + `"}]}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "opencode/1.0")

	_, err := svc.Forward(context.Background(), c, account, body)
	require.ErrorIs(t, err, ErrCodexSynthContextWindowExceeded)
	// Codex only compacts on a streamed response.failed with this code.
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))
	data := strings.TrimSpace(strings.SplitN(strings.SplitN(rec.Body.String(), "data: ", 2)[1], "\n", 2)[0])
	require.Equal(t, "response.failed", gjson.Get(data, "type").String())
	require.Equal(t, "context_length_exceeded", gjson.Get(data, "response.error.code").String())
	require.Equal(t, OpenAIContextLengthExceededMessage, gjson.Get(data, "response.error.message").String())
	require.True(t, IsResponseCommitted(c), "handler must not append a fallback response.failed")

	// Non-streamed requests get HTTP 400 JSON with the same code.
	bodyNoStream := []byte(`{"model":"gpt-5.5","stream":false,"input":[{"type":"message","role":"user","content":"` + huge + `"}]}`)
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(bodyNoStream))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "opencode/1.0")
	_, err = svc.Forward(context.Background(), c, account, bodyNoStream)
	require.ErrorIs(t, err, ErrCodexSynthContextWindowExceeded)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "context_length_exceeded", gjson.Get(rec.Body.String(), "error.code").String())
}
