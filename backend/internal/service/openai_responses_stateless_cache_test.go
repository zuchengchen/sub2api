package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const statelessCacheTestBody = `{"model":"gpt-6.1-sol","prompt_cache_key":"explicit-prefix","input":[{"role":"user","content":"text"}]}`

func statelessCacheContext() *gin.Context {
	c := responsesAffinityTestContext(811, 19, PlatformOpenAI)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c
}

func TestResponsesStatelessCacheTrialOriginalInput(t *testing.T) {
	const msg = `{"role":"user","content":[{"type":"input_text","text":"tail"}]}`
	const base = `{"model":"gpt-6.1-sol","prompt_cache_key":"explicit","input":`
	const summary = `{"type":"summary_text","text":"inline explanation"}`
	c := statelessCacheContext()
	for _, tc := range []struct {
		name, input string
		ok          bool
	}{
		{"single text", `"independent prompt"`, true},
		{"all roles", `[{"role":"system","content":"base"},{"role":"developer","content":"policy"},` + msg + `,{"role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}]`, true},
		{"complete tools", `[` + msg + `,` + autoContinuationCall + `,{"type":"function_call_output","call_id":"call_lookup","output":""}]`, true},
		{"custom tools", `[{"type":"custom_tool_call","name":"apply","call_id":"id","input":"patch"},{"type":"custom_tool_call_output","call_id":"id","output":"ok"}]`, true},
		{"complete reasoning", `[` + msg + `,{"type":"reasoning","id":"rs_fixture","summary":[],"encrypted_content":"opaque","status":"completed"},` + autoContinuationCall + `,` + autoContinuationTool + `]`, true},
		{"reasoning with summary", `[{"type":"reasoning","id":"rs_fixture","summary":[` + summary + `],"encrypted_content":"opaque"},` + msg + `]`, true},
		{"inline summary only", `[{"type":"reasoning","summary":[` + summary + `]},` + msg + `]`, true},
		{"reference reasoning", `[{"type":"reasoning","id":"rs_fixture","summary":[]},` + msg + `]`, false},
		{"empty reasoning", `[{"type":"reasoning","summary":[]},` + msg + `]`, false},
		{"hosted reasoning", `[{"type":"reasoning","summary":[],"encrypted_content":"opaque","file_id":"managed"},` + msg + `]`, false},
		{"bad summary", `[{"type":"reasoning","summary":[{"type":"summary_text","text":1}],"encrypted_content":"opaque"}]`, false},
		{"unfinished reasoning", `[{"type":"reasoning","summary":[],"encrypted_content":"opaque","status":"in_progress"}]`, false},
		{"missing call", `[` + msg + `,` + autoContinuationTool + `]`, false},
		{"missing result", `[` + msg + `,` + autoContinuationCall + `]`, false},
		{"duplicate call", `[` + autoContinuationCall + `,` + autoContinuationCall + `,` + autoContinuationTool + `]`, false},
		{"reference", `[{"type":"item_reference","id":"item"},` + msg + `]`, false},
		{"file reference", `[{"role":"user","content":[{"type":"input_file","file_id":"id"}]}]`, false},
		{"unresolved image", `[{"role":"user","content":[{"type":"input_image","image_url":"remote"}]}]`, false},
		{"hosted tool", `[{"type":"web_search_call","id":"ws_fixture"},` + msg + `]`, false},
		{"compact", `[{"type":"compaction","encrypted_content":"opaque"},` + msg + `]`, false},
		{"unknown input", `[{"type":"unexpected","id":"id"}]`, false},
		{"empty input", `[]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(base + tc.input + "}")
			require.Equal(t, tc.ok, responsesStatelessCacheKey(c, raw) != "")
		})
	}
	for _, suffix := range []string{
		`"previous_response_id":"resp_remote"`, `"conversation":{"id":"conv"}`, `"generate":true`,
		`"metadata":{"user_id":"private-session"}`, `"client_metadata":{"session_id":"session"}`,
		`"client_metadata":{"x-codex-turn-state":"opaque"}`, `"session_id":"root"`, `"store":true`,
		`"background":true`, `"context_management":[]`, `"prompt":{"id":"stored"}`,
		`"prompt_cache_key":"other"`, `"PrOmPt_CaChE_KeY":"case-conflict"`,
	} {
		body := strings.TrimSuffix(statelessCacheTestBody, "}") + "," + suffix + "}"
		require.Empty(t, responsesStatelessCacheKey(c, []byte(body)), suffix)
	}
	for _, name := range []string{"session-id", "thread-id", "turn-id", openAIWSTurnStateHeader, openAIWSTurnMetadataHeader, claudeCodeSessionHeader} {
		c.Request.Header.Set(name, "private")
		require.Empty(t, responsesStatelessCacheKey(c, []byte(statelessCacheTestBody)), name)
		c.Request.Header.Del(name)
	}
}

func TestResponsesStatelessCacheTrialScopeAndExpiry(t *testing.T) {
	end := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	for _, tc := range []struct{ mode, until, want string }{
		{"", "", ""}, {"all", end, ""}, {"key:811:unknown", end, ""},
		{"key:812:trial", end, ""}, {"key:0811:full", "", ""}, {"key:811:trial", "", ""},
		{"key:811:trial", "bad", ""}, {"key:811:trial", "2000-01-01T00:00:00Z", ""},
		{"key:811:trial", "2100-01-01T00:00:00+00:00", ""},
		{"key:811:trial", end, "trial"}, {"key:811:full", "", "fixed_full"},
	} {
		t.Run(tc.mode+"/"+tc.until, func(t *testing.T) {
			t.Setenv(responsesStatelessCacheModeEnv, tc.mode)
			t.Setenv(responsesStatelessCacheUntilEnv, tc.until)
			c := statelessCacheContext()
			stageResponsesStatelessCacheTrial(c, []byte(statelessCacheTestBody), "gpt-6.1-sol")
			raw, staged := c.Get(responsesStatelessCacheRequestKey)
			require.Equal(t, tc.want != "", staged)
			if !staged {
				return
			}
			trial := raw.(*responsesStatelessCacheRequest)
			if tc.want == "trial" {
				require.Contains(t, []string{"trial_full", "control_prewarm"}, trial.arm)
			} else {
				require.Equal(t, tc.want, trial.arm)
			}
			stageResponsesStatelessCacheTrial(c, []byte(statelessCacheTestBody), "gpt-6.1-sol")
			next, _ := c.Get(responsesStatelessCacheRequestKey)
			require.Same(t, trial, next)
			assignment, action := responsesStatelessCacheTrialObservation(c)
			require.Empty(t, assignment)
			require.Empty(t, action)
		})
	}
	t.Setenv(responsesStatelessCacheModeEnv, "key:811:full")
	for _, mutate := range []func(*gin.Context){
		func(c *gin.Context) { c.Request.Method = http.MethodGet },
		func(c *gin.Context) { c.Request.URL.Path = "/v1/messages" },
		func(c *gin.Context) { c.Request.URL.Path = "/v1/responses/compact" },
		func(c *gin.Context) { c.Request.Header.Set("User-Agent", "codex_cli_rs/0.164.0") },
		func(c *gin.Context) { MarkOpenAINativeCompactionV2(c) },
		func(c *gin.Context) { SetOpenAIImageIntentHint(c, true) },
		func(c *gin.Context) { SetOpenAIClientTransport(c, OpenAIClientTransportWS) },
		func(c *gin.Context) { c.Set("api_key", &APIKey{ID: 811}) },
	} {
		c := statelessCacheContext()
		mutate(c)
		stageResponsesStatelessCacheTrial(c, []byte(statelessCacheTestBody), "gpt-6.1-sol")
		_, staged := c.Get(responsesStatelessCacheRequestKey)
		require.False(t, staged)
	}
}

func TestResponsesStatelessCacheTrialUsesActualAccountAndModel(t *testing.T) {
	t.Setenv(responsesStatelessCacheModeEnv, "key:811:full")
	for _, owner := range []string{"", "upstream-owner"} {
		c := statelessCacheContext()
		stageResponsesStatelessCacheTrial(c, []byte(statelessCacheTestBody), "gpt-6.1-sol")
		account := &Account{ID: 18, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": owner}}
		pck := scopeCodexAccountIdentityValue(account, 811, "prompt-cache", "explicit-prefix")
		payload := map[string]any{"model": "gpt-6.1-sol", "prompt_cache_key": pck}
		try := func() *responsesStatelessCacheAttempt {
			return responsesStatelessCacheForAttempt(c, account, payload, "", false)
		}
		require.NotNil(t, try())
		require.Nil(t, responsesStatelessCacheForAttempt(c, account, payload, "private-scope", false))
		require.Nil(t, responsesStatelessCacheForAttempt(c, account, payload, "", true))
		payload["model"] = "other-model"
		require.Nil(t, try())
		payload["model"] = "gpt-6.1-sol"
		payload["prompt_cache_key"] = "different-final-key"
		require.Nil(t, try())
		payload["prompt_cache_key"] = pck
		account.Type = AccountTypeSetupToken
		require.Nil(t, try())
		account.Type = AccountTypeOAuth
		c.Set("api_key", &APIKey{ID: 812, UserID: 19, Group: &Group{Platform: PlatformOpenAI}})
		require.Nil(t, try())
	}
}

func TestResponsesStatelessCacheTrialRetainsAssignmentOnHTTPFallback(t *testing.T) {
	t.Setenv(responsesStatelessCacheModeEnv, "key:811:trial")
	t.Setenv(responsesStatelessCacheUntilEnv, time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired)
	}))
	defer server.Close()
	for _, arm := range []string{"control_prewarm", "trial_full"} {
		t.Run(arm, func(t *testing.T) {
			svc, account, upstream := httpBridgeFixture(t, server.URL)
			svc.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = true
			c := statelessCacheContext()
			body := []byte(statelessCacheTestBody)
			svc.GenerateResponsesAccountSessionHash(c, body, "gpt-6.1-sol")
			raw, staged := c.Get(responsesStatelessCacheRequestKey)
			require.True(t, staged)
			raw.(*responsesStatelessCacheRequest).arm = arm
			result, err := svc.Forward(c.Request.Context(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.False(t, result.OpenAIWSMode)
			require.Len(t, upstream.bodies, 1)
			assignment, action := responsesStatelessCacheTrialObservation(c)
			require.Equal(t, arm, assignment)
			require.Empty(t, action, "a failed WS handshake wrote no frames")
		})
	}
}

func TestResponsesStatelessCacheTrialOnlyAfterFreshPrewarmReservation(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenAIWS.PrewarmGenerateEnabled = true
	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	run := func(t *testing.T, warm, reused bool, previous, arm string) {
		c := statelessCacheContext()
		capture := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_fake","output":[]}}`)}}
		conn := newOpenAIWSConn(fmt.Sprintf("lease-%v-%v-%s", warm, reused, previous), account.ID, capture, nil)
		lease := &openAIWSConnLease{conn: conn, reused: reused}
		if warm {
			lease.MarkPrewarmed()
		}
		trial := &responsesStatelessCacheAttempt{arm: arm}
		c.Set(responsesStatelessCacheAssignmentKey, arm) // admitted before dialing
		payload := map[string]any{"model": "gpt-6.1-sol", "input": "full"}
		result, err := svc.performOpenAIWSGeneratePrewarm(context.Background(), c, lease, OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, payload, previous, nil, nil, account, nil, 0, trial)
		require.NoError(t, err)
		assignment, action := responsesStatelessCacheTrialObservation(c)
		require.Equal(t, arm, assignment)
		if warm || reused || previous != "" {
			require.Empty(t, action)
			require.False(t, trial.skipped)
			return
		}
		if arm == "control_prewarm" {
			require.NotNil(t, result)
			require.Equal(t, "resp_fake", result.ResponseID)
			require.Equal(t, "prewarm_written", action)
			return
		}
		require.True(t, trial.skipped)
		require.Empty(t, action, "a full input is counted only after its business frame writes")
		require.Nil(t, result)
		require.NotContains(t, payload, "previous_response_id")
	}
	run(t, false, false, "", "control_prewarm")
	run(t, false, false, "", "trial_full")
	run(t, true, false, "", "trial_full")
	run(t, false, true, "", "fixed_full")
	run(t, false, false, "resp_existing", "trial_full")
}
