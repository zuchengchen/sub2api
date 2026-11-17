//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type oauthReliabilityContractHandlerUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	wire       string
	onFirst    func()
}

func (u *oauthReliabilityContractHandlerUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if first && u.onFirst != nil {
		u.onFirst()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"synthetic-handler-contract"}},
		Body:       io.NopCloser(strings.NewReader(u.wire)),
	}, nil
}

func (u *oauthReliabilityContractHandlerUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func TestOAuthReliabilityContractChatHandlerEOFReplayBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, wire     string
		wantAttempts   int
		alreadyWritten bool
		cancelInFlight bool
	}{
		{"empty_eof", "", 2, false, false},
		{"metadata_eof", "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_contract_meta\"}}\n\n", 2, false, false},
		{"upstream_heartbeat_eof", ": keepalive\n\n", 2, false, false},
		{"semantic_output", "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_contract\",\"output_index\":0,\"content_index\":0,\"delta\":\"A synthetic answer that is already being delivered.\"}\n\n", 1, false, false},
		{"hosted_tool", "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"ws_contract\",\"type\":\"web_search_call\",\"status\":\"in_progress\"}}\n\n", 1, false, false},
		{"downstream_committed", "", 1, true, false},
		{"canceled_inflight", "", 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requestCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			upstream := &oauthReliabilityContractHandlerUpstream{wire: tc.wire}
			if tc.cancelInFlight {
				upstream.onFirst = cancel
			}
			handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
			handler.maxAccountSwitches = 1
			c, recorder := newOpenAIResponsesFailoverTestContext(t, requestCtx)
			body := []byte(`{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"synthetic contract"}]}`)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)).WithContext(requestCtx)
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.alreadyWritten {
				// A queued/previous protocol write is a commitment boundary even
				// though this attempt has not emitted any model text.
				_, err := c.Writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"prior output\"}}]}\n\n"))
				require.NoError(t, err)
				service.MarkResponseCommitted(c)
			}
			handler.ChatCompletions(c)
			require.Len(t, upstream.calls(), tc.wantAttempts, "retry is controlled by the real outer handler, including the existing account-switch budget")
			if tc.wantAttempts == 2 {
				require.Equal(t, []int64{1, 2}, upstream.calls())
				require.Contains(t, recorder.Body.String(), "error", "exhausting the two accounts must produce a failed response")
			}
			if tc.cancelInFlight {
				require.NotContains(t, recorder.Body.String(), "chat.completion.chunk", "cancellation must not fabricate a completed reply")
			}
		})
	}
}

func TestOAuthReliabilityContractHandlerDeterministic400IsTerminal(t *testing.T) {
	for _, route := range []string{"responses", "chat", "messages"} {
		t.Run(route, func(t *testing.T) {
			upstream := &oauthReliabilityContractHandlerUpstream{wire: "data: " + `{"type":"response.failed","response":{"id":"resp_contract_failed","status":"failed","output":[],"error":{"type":"invalid_request_error","code":"invalid_function_parameters","param":"tools[0].parameters","message":"Invalid schema: additionalProperties must be false."}}}` + "\n\n"}
			handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
			c, recorder := newOpenAIResponsesFailoverTestContext(t, context.Background())
			apiKey, ok := middleware2.GetAPIKeyFromContext(c)
			require.True(t, ok)
			apiKey.Group.AllowMessagesDispatch = true
			var body string
			switch route {
			case "responses":
				body = `{"model":"gpt-5.1","stream":false,"input":"synthetic contract"}`
			case "chat":
				body = `{"model":"gpt-5.1","stream":false,"messages":[{"role":"user","content":"synthetic contract"}]}`
			default:
				body = `{"model":"gpt-5.1","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"synthetic contract"}]}`
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+route, strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			switch route {
			case "responses":
				handler.Responses(c)
			case "chat":
				handler.ChatCompletions(c)
			default:
				handler.Messages(c)
			}
			require.Equal(t, []int64{1}, upstream.calls(), "parameter400 must not reach the second OAuth account")
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, recorder.Body.String(), "invalid_request_error")
			require.True(t, json.Valid(recorder.Body.Bytes()), "the handler must return one complete error object without appending a generic502 response")
		})
	}
}
