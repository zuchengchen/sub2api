//go:build unit

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

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const messagesEncryptedRecoveryCipher = "CAQSsynthetic-messages-signature"
const messagesEncryptedRecoveryMessage = "The encrypted content CAQSsynthetic-messages-signature could not be verified. Reason: Encrypted content could not be decrypted or parsed."

type messagesEncryptedRecoveryUpstream struct {
	httpUpstreamRecorder
	accountIDs []int64
	afterFirst func()
}

func (u *messagesEncryptedRecoveryUpstream) Do(req *http.Request, proxyURL string, accountID int64, concurrency int) (*http.Response, error) {
	u.accountIDs = append(u.accountIDs, accountID)
	resp, err := u.httpUpstreamRecorder.Do(req, proxyURL, accountID, concurrency)
	// Snapshot final transport headers before recovery can mutate a request.
	u.requests[len(u.requests)-1] = req.Clone(req.Context())
	if len(u.accountIDs) == 1 && u.afterFirst != nil {
		u.afterFirst()
	}
	return resp, err
}

func (u *messagesEncryptedRecoveryUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func messagesEncryptedRecoveryBody(stream, encrypted bool) []byte {
	thinking := ""
	if encrypted {
		thinking = `{"type":"thinking","thinking":"synthetic plan","signature":"` + messagesEncryptedRecoveryCipher + `"},`
	}
	return []byte(fmt.Sprintf(`{
		"model":"gpt-6.1-sol","max_tokens":32,"stream":%t,
		"system":[{"type":"text","text":"Synthetic test instructions.","cache_control":{"type":"ephemeral","ttl":"5m"}}],
		"metadata":{"user_id":"synthetic_session_1669213a-22c0-4eaf-a891-21fcd33e55a1"},
		"messages":[
			{"role":"user","content":"Read the synthetic fixture."},
			{"role":"assistant","content":[%s{"type":"text","text":"I will inspect the fixture."},{"type":"tool_use","id":"toolu_synthetic_keep","name":"Read","input":{"path":"synthetic-fixture.txt","sequence":9007199254740993}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_synthetic_keep","content":"Synthetic fixture result."}]}
		],
		"tools":[{"name":"Read","description":"Read the synthetic fixture","input_schema":{"type":"object","properties":{"path":{"type":"string"},"sequence":{"type":"integer"}},"required":["path"]},"cache_control":{"type":"ephemeral","ttl":"5m"}}]
	}`, stream, thinking))
}

func messagesEncryptedRecoveryError(code, message string) string {
	fields := map[string]any{"type": "invalid_request_error", "message": message}
	if code != "" {
		fields["code"] = code
	}
	body, _ := json.Marshal(map[string]any{"error": fields})
	return string(body)
}

func messagesEncryptedRecoveryFixture(body []byte, status int, rejection string) (*OpenAIGatewayService, *Account, *gin.Context, *messagesEncryptedRecoveryUpstream) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "baocode")
	c.Request.Header.Set("session_id", "1669213a-22c0-4eaf-a891-21fcd33e55a1")
	upstream := &messagesEncryptedRecoveryUpstream{httpUpstreamRecorder: httpUpstreamRecorder{
		responses: []*http.Response{
			newOpenAIRejectedFieldTestResponse(status, rejection),
			openAICompatSSECompletedResponse("resp_synthetic_encrypted_recovered", "gpt-6.1-sol"),
		},
		resp: newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, rejection),
	}}
	return &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream},
		newOpenAIOAuthNamespaceTestAccount(), c, upstream
}

func requireMessagesEncryptedReplayPreserved(t *testing.T, upstream *messagesEncryptedRecoveryUpstream, account *Account) {
	t.Helper()
	require.Equal(t, 2, len(upstream.bodies), "exactly one recovery attempt")
	first, second := upstream.bodies[0], upstream.bodies[1]
	require.Equal(t, messagesEncryptedRecoveryCipher, gjson.GetBytes(first, `input.#(type=="reasoning").encrypted_content`).String())
	require.False(t, gjson.GetBytes(second, `input.#(type=="reasoning")`).Exists(), "empty reasoning skeleton is not valid replay input")
	var expected, actual map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(first, &expected))
	require.NoError(t, json.Unmarshal(second, &actual))
	var input []json.RawMessage
	require.NoError(t, json.Unmarshal(expected["input"], &input))
	kept := make([]json.RawMessage, 0, len(input)-1)
	removed := 0
	for _, item := range input {
		if gjson.GetBytes(item, "type").String() == "reasoning" {
			removed++
			continue
		}
		kept = append(kept, item)
	}
	require.Equal(t, 1, removed)
	expected["input"], _ = json.Marshal(kept)
	expectedJSON, _ := json.Marshal(expected)
	actualJSON, _ := json.Marshal(actual)
	require.JSONEq(t, string(expectedJSON), string(actualJSON), "only the rejected opaque reasoning item may change")
	require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(second, "model").String())
	require.Equal(t, "Read", gjson.GetBytes(second, "tools.0.name").String())
	require.Equal(t, "toolu_synthetic_keep", gjson.GetBytes(second, `input.#(type=="function_call").call_id`).String())
	require.Equal(t, "toolu_synthetic_keep", gjson.GetBytes(second, `input.#(type=="function_call_output").call_id`).String())
	arguments := gjson.GetBytes(first, `input.#(type=="function_call").arguments`).String()
	require.Equal(t, "9007199254740993", gjson.Get(arguments, "sequence").Raw)
	require.Equal(t, arguments, gjson.GetBytes(second, `input.#(type=="function_call").arguments`).String(), "opaque tool arguments must retain their exact bytes")
	require.Contains(t, string(second), "Synthetic fixture result.")
	require.Contains(t, string(second), "Synthetic test instructions.")
	require.NotEmpty(t, gjson.GetBytes(first, "prompt_cache_key").String())
	require.Equal(t, []int64{account.ID, account.ID}, upstream.accountIDs)
	require.Equal(t, upstream.requests[0].URL.String(), upstream.requests[1].URL.String())
	require.Equal(t, upstream.requests[0].Host, upstream.requests[1].Host)
	require.Equal(t, upstream.requests[0].Header, upstream.requests[1].Header, "auth, cache and session headers must survive recovery unchanged")
	require.Equal(t, int64(len(second)), upstream.requests[1].ContentLength)
	require.NotNil(t, upstream.requests[1].GetBody)
	replay, err := upstream.requests[1].GetBody()
	require.NoError(t, err)
	replayBody, err := io.ReadAll(replay)
	require.NoError(t, err)
	require.NoError(t, replay.Close())
	require.Equal(t, string(second), string(replayBody), "retry transport replay source must match its body")
}

func TestMessagesEncryptedRecoveryExact400(t *testing.T) {
	for _, tc := range []struct{ name, code, message string }{
		{"explicit_code", "invalid_encrypted_content", messagesEncryptedRecoveryMessage},
		{"missing_code", "", messagesEncryptedRecoveryMessage},
		{"signature_code", "thinking_signature_invalid", messagesEncryptedRecoveryMessage},
		{"hydration_message", "", strings.Replace(messagesEncryptedRecoveryMessage, "Reason: ", "Reason: encrypted content hydration failed: ", 1)},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				body := messagesEncryptedRecoveryBody(stream, true)
				svc, account, c, upstream := messagesEncryptedRecoveryFixture(body, http.StatusBadRequest, messagesEncryptedRecoveryError(tc.code, tc.message))
				result, err := svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "synthetic-stable-cache", "")
				require.NoError(t, err)
				require.NotNil(t, result)
				requireMessagesEncryptedReplayPreserved(t, upstream, account)
			})
		}
	}
}

func TestMessagesEncryptedRecoveryOnlyOnceAndNoPrestrip(t *testing.T) {
	rejection := messagesEncryptedRecoveryError("invalid_encrypted_content", messagesEncryptedRecoveryMessage)
	t.Run("second_400_is_final", func(t *testing.T) {
		body := messagesEncryptedRecoveryBody(false, true)
		svc, account, c, upstream := messagesEncryptedRecoveryFixture(body, http.StatusBadRequest, rejection)
		upstream.responses[1] = newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, rejection)
		_, err := svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "synthetic-stable-cache", "")
		require.Error(t, err)
		requireMessagesEncryptedReplayPreserved(t, upstream, account)
	})
	t.Run("successful_first_attempt_preserves_cipher", func(t *testing.T) {
		body := messagesEncryptedRecoveryBody(false, true)
		svc, account, c, upstream := messagesEncryptedRecoveryFixture(body, http.StatusBadRequest, rejection)
		upstream.responses = []*http.Response{openAICompatSSECompletedResponse("resp_synthetic_direct", "gpt-6.1-sol")}
		result, err := svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "synthetic-stable-cache", "")
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Len(t, upstream.bodies, 1)
		require.Equal(t, messagesEncryptedRecoveryCipher, gjson.GetBytes(upstream.bodies[0], `input.#(type=="reasoning").encrypted_content`).String())
	})
	t.Run("no_cipher_does_not_retry", func(t *testing.T) {
		body := messagesEncryptedRecoveryBody(false, false)
		svc, account, c, upstream := messagesEncryptedRecoveryFixture(body, http.StatusBadRequest, rejection)
		_, err := svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "synthetic-stable-cache", "")
		require.Error(t, err)
		require.Len(t, upstream.bodies, 1)
	})
}

func TestMessagesEncryptedRecoverySetupTokenUsesSameOAuthContract(t *testing.T) {
	body := messagesEncryptedRecoveryBody(false, true)
	svc, account, c, upstream := messagesEncryptedRecoveryFixture(body, http.StatusBadRequest,
		messagesEncryptedRecoveryError("invalid_encrypted_content", messagesEncryptedRecoveryMessage))
	account.Type = AccountTypeSetupToken
	result, err := svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "synthetic-stable-cache", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	requireMessagesEncryptedReplayPreserved(t, upstream, account)
}

func TestMessagesEncryptedRecoveryDoesNotReplayAfterCancellationOrOutput(t *testing.T) {
	for _, mode := range []string{"call_context_canceled", "request_context_canceled", "headers_committed", "body_written"} {
		t.Run(mode, func(t *testing.T) {
			body := messagesEncryptedRecoveryBody(false, true)
			svc, account, c, upstream := messagesEncryptedRecoveryFixture(body, http.StatusBadRequest, messagesEncryptedRecoveryError("invalid_encrypted_content", messagesEncryptedRecoveryMessage))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			c.Request = c.Request.WithContext(requestCtx)
			upstream.afterFirst = func() {
				switch mode {
				case "call_context_canceled":
					cancel()
				case "request_context_canceled":
					cancelRequest()
				case "headers_committed":
					c.Writer.WriteHeaderNow()
				case "body_written":
					_, _ = c.Writer.Write([]byte("already visible synthetic output"))
				}
			}
			_, err := svc.ForwardAsAnthropic(ctx, c, account, body, "synthetic-stable-cache", "")
			require.Error(t, err)
			require.Len(t, upstream.bodies, 1, "replay must stop after client cancellation or committed output")
		})
	}
}

func TestMessagesEncryptedRecoveryRejectsUnsafeUpstreamResponses(t *testing.T) {
	exact := messagesEncryptedRecoveryError("invalid_encrypted_content", messagesEncryptedRecoveryMessage)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unrelated_400", 400, messagesEncryptedRecoveryError("invalid_request_error", "Bad Request")},
		{"unrelated_signature", 400, messagesEncryptedRecoveryError("thinking_signature_invalid", "Invalid signature")},
		{"wrong_code", 400, messagesEncryptedRecoveryError("invalid_api_key", messagesEncryptedRecoveryMessage)},
		{"unauthorized", 401, exact}, {"forbidden", 403, exact}, {"rate_limit", 429, exact},
		{"root_usage", 400, strings.TrimSuffix(exact, "}") + `,"usage":{"input_tokens":1}}`},
		{"root_output", 400, strings.TrimSuffix(exact, "}") + `,"output":[{"type":"message","content":[{"type":"output_text","text":"visible"}]}]}`},
		{"response_usage", 400, strings.TrimSuffix(exact, "}") + `,"response":{"usage":{"input_tokens":1}}}`},
		{"response_tool_usage", 400, strings.TrimSuffix(exact, "}") + `,"response":{"tool_usage":{"image_generation":{"output_tokens":1}}}}`},
		{"nested_echo_message", 400, `{"error":{"message":"Bad Request","debug":` + exact + `}}`},
		{"root_echo_message", 400, `{"error":{"message":"Bad Request"},"echo":` + exact + `}`},
		{"text_prefix_not_exact", 400, messagesEncryptedRecoveryError("", "Quoted example: "+messagesEncryptedRecoveryMessage)},
		{"malformed_json", 400, strings.TrimSuffix(exact, "}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := messagesEncryptedRecoveryBody(false, true)
			svc, account, c, upstream := messagesEncryptedRecoveryFixture(body, tc.status, tc.body)
			_, err := svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "synthetic-stable-cache", "")
			require.Error(t, err)
			require.Len(t, upstream.bodies, 1, "unsafe or unrelated upstream errors must not replay the request")
			require.Equal(t, messagesEncryptedRecoveryCipher, gjson.GetBytes(upstream.bodies[0], `input.#(type=="reasoning").encrypted_content`).String())
		})
	}
}
