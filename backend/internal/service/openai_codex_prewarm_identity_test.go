package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// e54a5713 core/src/responses_metadata.rs preserves Some("") / None and
// serializes turn_started_at_unix_ms only when a turn has supplied it.
func TestCodexFingerprintNativePrewarmTurnShape(t *testing.T) {
	for _, tc := range []struct {
		name, kind, turn string
		omitted, started bool
	}{
		{name: "empty_startup", kind: "prewarm"},
		{name: "omitted_startup", kind: "prewarm", omitted: true},
		{name: "prewarm_during_turn", kind: "prewarm", turn: seededWSRawTurn, started: true},
		{name: "normal_turn", kind: "turn", turn: seededWSRawTurn, started: true},
		{name: "generic_turn_without_id", kind: "turn", omitted: true},
	} {
		for _, carrier := range []string{"raw", "map", "header"} {
			t.Run(tc.name+"/"+carrier, func(t *testing.T) {
				cm := seededWSMetadata("a1e48840-2a1a-44ad-8890-bfa930340c78", true)
				var embedded map[string]any
				require.NoError(t, json.Unmarshal([]byte(cm[openAIWSTurnMetadataHeader].(string)), &embedded))
				embedded["request_kind"] = tc.kind
				if tc.omitted {
					delete(cm, "turn_id")
					delete(embedded, "turn_id")
				} else {
					cm["turn_id"] = tc.turn
					embedded["turn_id"] = tc.turn
				}
				if tc.started {
					embedded["turn_started_at_unix_ms"] = int64(1794268729796)
				}
				encoded, err := json.Marshal(embedded)
				require.NoError(t, err)
				cm[openAIWSTurnMetadataHeader] = string(encoded)
				headers := http.Header{"X-Codex-Turn-Metadata": {string(encoded)}, "Session-Id": {seededWSRawSession}}
				account := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "acct-golden", "chatgpt_user_id": "user-golden"}, Extra: map[string]any{"codex_fingerprint_mode": "full", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111", "codex_experimental_fingerprint_convergence": true}}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/responses", nil)
				c.Request.Header = headers.Clone()
				c.Set("api_key", &APIKey{ID: 1, UserID: 1, User: &User{ID: 1}})
				body := map[string]any{"model": "gpt-5.5", "client_metadata": cm, "input": []any{}, "prompt_cache_key": seededWSRawSession}
				var ids *codexFingerprintIDs
				var output gjson.Result
				switch carrier {
				case "raw":
					raw, e := json.Marshal(body)
					require.NoError(t, e)
					ids = resolveCodexFingerprintIDsForRawBody(c, account, nil, raw)
					out, changed, e := applyCodexFingerprintClientMetadataRaw(raw, ids)
					require.NoError(t, e)
					require.True(t, changed)
					output = gjson.GetBytes(out, "client_metadata")
				case "map":
					ids = resolveCodexFingerprintIDsForDecodedBody(c, account, nil, body)
					require.True(t, applyCodexFingerprintClientMetadata(body, ids))
					out, e := json.Marshal(body)
					require.NoError(t, e)
					output = gjson.GetBytes(out, "client_metadata")
				case "header":
					ids = resolveCodexFingerprintIDsWithBody(c, account, headers, nil)
				}
				require.NotNil(t, ids)
				applyCodexFingerprintHeaders(headers, ids)
				headerMeta := gjson.Parse(headers.Get(openAIWSTurnMetadataHeader))
				metadata := []gjson.Result{headerMeta}
				if carrier != "header" {
					metadata = append(metadata, output, gjson.Parse(output.Get(openAIWSTurnMetadataHeader).String()))
					require.Equal(t, seededWSExpectedInstallation, output.Get("x-codex-installation-id").String())
				}
				require.Equal(t, seededWSExpectedInstallation, headerMeta.Get("installation_id").String())
				require.NotEqual(t, seededWSRawSession, headerMeta.Get("session_id").String())
				for _, meta := range metadata {
					if tc.kind == "prewarm" && tc.turn == "" {
						require.Equal(t, !tc.omitted, meta.Get("turn_id").Exists(), "preserve omitted versus explicit empty turn")
						require.Empty(t, meta.Get("turn_id").String())
						require.False(t, meta.Get("turn_started_at_unix_ms").Exists(), "startup prewarm must not invent a turn timestamp")
					} else {
						require.NotEmpty(t, meta.Get("turn_id").String())
						require.Equal(t, ids.turnID, meta.Get("turn_id").String())
						if tc.turn != "" {
							require.NotEqual(t, tc.turn, meta.Get("turn_id").String())
						}
					}
				}
				if tc.started {
					require.Equal(t, int64(1794268729796), headerMeta.Get("turn_started_at_unix_ms").Int())
				}
				if tc.kind == "turn" && tc.turn == "" {
					require.Positive(t, headerMeta.Get("turn_started_at_unix_ms").Int(), "generic clients retain turn synthesis")
				}
			})
		}
	}
}

func TestCodexOAuthWSNativePrewarmTurnShape(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			headers, frames := runSeededWSIdentitySession(t, mode, "a1e48840-2a1a-44ad-8890-bfa930340c78", 1, true)
			require.Len(t, frames, 2)
			cm := gjson.GetBytes(frames[0], "client_metadata")
			for _, meta := range []gjson.Result{cm, gjson.Parse(cm.Get(openAIWSTurnMetadataHeader).String()), gjson.Parse(headers.Get(openAIWSTurnMetadataHeader))} {
				require.True(t, meta.Get("turn_id").Exists())
				require.Empty(t, meta.Get("turn_id").String())
				require.False(t, meta.Get("turn_started_at_unix_ms").Exists())
			}
			later := gjson.GetBytes(frames[1], "client_metadata")
			laterMeta := gjson.Parse(later.Get(openAIWSTurnMetadataHeader).String())
			require.NotEmpty(t, later.Get("turn_id").String())
			require.NotEqual(t, seededWSRawTurn, later.Get("turn_id").String())
			require.Equal(t, later.Get("turn_id").String(), laterMeta.Get("turn_id").String())
			require.Equal(t, int64(1794268729796), laterMeta.Get("turn_started_at_unix_ms").Int())
		})
	}
}
