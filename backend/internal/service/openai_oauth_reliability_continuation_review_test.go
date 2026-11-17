//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityReviewContinuationAPIKeyEndpointIsolation(t *testing.T) {
	account := newOpenAIRejectedFieldTestAccount()
	delete(account.Credentials, "base_url")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(200, `{"id":"resp_endpoint_owner","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`),
		newOpenAIRejectedFieldTestResponse(200, `{"id":"resp_other_endpoint","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`),
	}}
	svc := newOpenAIRejectedFieldTestService(upstream)
	first := []byte(`{"model":"gpt-5.5","stream":false,"input":"root"}`)
	result, err := svc.Forward(context.Background(), responseAuthOwnerContext(101, 201), account, first)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "api.openai.com", upstream.lastReq.URL.Host)
	current := snapshotOAuthRefreshAccount(account)
	current.Credentials["base_url"] = "https://api.anthropic.com"
	require.NotEqual(t, account.GetOpenAIBaseURL(), current.GetOpenAIBaseURL(), "these are different actual destinations")
	second := []byte(`{"model":"gpt-5.5","stream":false,"previous_response_id":"resp_endpoint_owner","input":"delta"}`)
	result, err = svc.Forward(context.Background(), responseAuthOwnerContext(101, 201), current, second)
	require.ErrorIs(t, err, errOpenAIResponseAuthOwnerMismatch)
	require.Nil(t, result)
	require.Len(t, upstream.bodies, 1, "a default-endpoint hash collision must not send the known anchor to another endpoint")
}

func TestOAuthReliabilityReviewContinuationToolSearchNativeSchema(t *testing.T) {
	for _, arguments := range []string{`{"query":"lookup"}`, `["lookup"]`, `"lookup"`, `null`} {
		for _, status := range []string{"", `,"status":"completed"`} {
			t.Run(fmt.Sprintf("args=%s/status=%s", arguments, status), func(t *testing.T) {
				// The official generated wire types allow any JSON arguments;
				// tool_search_output.status is optional. Unknown tool semantics
				// must be validated upstream, not mislabeled missing context.
				input := `[{"type":"tool_search_call","call_id":"call_search","execution":"client","arguments":` + arguments + `},{"type":"tool_search_output","call_id":"call_search","execution":"client","tools":[]` + status + `}]`
				body := []byte(`{"model":"gpt-5.5","stream":false,"input":` + input + `}`)
				upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(200, `{"id":"resp_search","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)}
				result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIRejectedFieldTestAccount(), body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				require.JSONEq(t, input, gjson.GetBytes(upstream.lastBody, "input").Raw)
			})
		}
	}
}

func TestOAuthReliabilityReviewContinuationConversationIsNativeState(t *testing.T) {
	for _, conversation := range []string{`"conv_native"`, `{"id":"conv_native"}`} {
		t.Run(conversation, func(t *testing.T) {
			text := []byte(`{"conversation":` + conversation + `,"input":"delta"}`)
			require.False(t, OpenAIContinuationMaySwitchAccount(text), "stored conversation history is not portable inline input")
			require.False(t, openAIContinuationStartsCompleteHistory(text), "stored conversation is not a locally observed complete root")
			body := []byte(`{"model":"gpt-5.5","stream":false,"conversation":` + conversation + `,"input":[{"type":"function_call_output","call_id":"call_stored_in_conversation","output":"native result"}]}`)
			upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(200, `{"id":"resp_conversation","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)}
			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIRejectedFieldTestAccount(), body)
			require.NoError(t, err, "the native API resolves the stored call from conversation")
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, conversation, gjson.GetBytes(upstream.lastBody, "conversation").Raw)
			require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(upstream.lastBody, "input").Raw)
		})
	}
}

func TestOAuthReliabilityReviewContinuationSparseCollectorHonorsIndexes(t *testing.T) {
	for _, mode := range []string{"out_of_order", "gap", "duplicate_index", "unknown_index", "terminal_index_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			collector := &openAIWSToolCallReplayCollector{}
			first := `{"type":"message","id":"msg_first","role":"assistant","status":"completed","content":[{"type":"output_text","text":"first decision"}]}`
			last := `{"type":"message","id":"msg_last","role":"assistant","status":"completed","content":[{"type":"output_text","text":"last correction"}]}`
			events := []string{`{"type":"response.output_item.done","output_index":1,"item":` + last + `}`}
			switch mode {
			case "out_of_order":
				events = append(events, `{"type":"response.output_item.done","output_index":0,"item":`+first+`}`)
			case "duplicate_index":
				events = append(events, `{"type":"response.output_item.done","output_index":1,"item":`+first+`}`)
			case "unknown_index":
				events = []string{`{"type":"response.output_item.done","item":` + first + `}`}
			}
			if mode == "terminal_index_mismatch" {
				events = append(events, `{"type":"response.completed","response":{"id":"resp_sparse","status":"completed","output":[`+last+`,`+first+`]}}`)
			} else {
				events = append(events, `{"type":"response.completed","response":{"id":"resp_sparse","status":"completed"}}`)
			}
			for _, event := range events {
				collector.AddEvent(gjson.Get(event, "type").String(), []byte(event))
			}
			if mode != "out_of_order" {
				require.False(t, collector.Complete(), "no full terminal array can repair missing or contradictory indexes")
				return
			}
			require.True(t, collector.Complete())
			items := collector.AllItems()
			require.Len(t, items, 2)
			require.Equal(t, "msg_first", gjson.GetBytes(items[0], "id").String())
			require.Equal(t, "msg_last", gjson.GetBytes(items[1], "id").String())
		})
	}
}

func TestOAuthReliabilityReviewContinuationBridgeSparseOutputPreservesHistory(t *testing.T) {
	for _, gap := range []bool{false, true} {
		t.Run(fmt.Sprintf("gap=%t", gap), func(t *testing.T) {
			cfg := diagnosticContinuationConfig()
			wire := "data: " + `{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_last","role":"assistant","status":"completed","content":[{"type":"output_text","text":"last correction"}]}}` + "\n\n"
			if !gap {
				wire += "data: " + `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_first","role":"assistant","status":"completed","content":[{"type":"output_text","text":"first decision"}]}}` + "\n\n"
			}
			wire += "data: " + `{"type":"response.completed","response":{"id":"resp_sparse","model":"gpt-6-astra","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
			first := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{first, diagnosticContinuationSSE("resp_next", `[]`)}}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			server, done := startPassthroughLifecycleServer(t, ctx, svc, diagnosticContinuationAccount())
			defer server.Close()
			client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-6-astra","input":[{"role":"user","content":"root"}]}`)
			defer client.CloseNow()
			for {
				_, frame, err := client.Read(ctx)
				require.NoError(t, err)
				if gjson.GetBytes(frame, "type").String() == "response.completed" {
					break
				}
			}
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_sparse","input":[{"role":"user","content":"continue"}]}`)))
			_, _, nextErr := client.Read(ctx)
			_ = client.CloseNow()
			var relayErr error
			select {
			case relayErr = <-done:
			case <-ctx.Done():
				t.Fatal("bridge did not stop")
			}
			if gap {
				require.Error(t, nextErr)
				var contextErr *OpenAIContinuationContextError
				require.True(t, errors.As(relayErr, &contextErr))
				require.Len(t, upstream.bodies, 1, "missing indexed output cannot authorize dropping the anchor")
				return
			}
			require.NoError(t, nextErr)
			require.Len(t, upstream.bodies, 2)
			input := gjson.GetBytes(upstream.bodies[1], "input").Array()
			require.Len(t, input, 4)
			require.Equal(t, "user", input[0].Get("role").String())
			require.Contains(t, input[0].Get("content").Raw, "root")
			require.Equal(t, "first decision", input[1].Get("content.0.text").String())
			require.Equal(t, "last correction", input[2].Get("content.0.text").String())
			require.Equal(t, "user", input[3].Get("role").String())
			require.Contains(t, input[3].Get("content").Raw, "continue")
		})
	}
}

func TestOAuthReliabilityReviewContinuationUnknownAndReferencesStayUnproven(t *testing.T) {
	for _, input := range []string{
		`[{"type":"future_tool_output","call_id":"future","output":"keep"}]`,
		`[{"type":"item_reference","id":"remote"}]`,
		`[{"id":"remote"}]`,
		`[{"type":"local_shell_call_output","id":"legacy","output":"keep"}]`,
		`[{"type":"program_output","call_id":"program","result":"keep","status":"completed"}]`,
	} {
		body := []byte(`{"input":` + input + `}`)
		require.False(t, OpenAIContinuationMaySwitchAccount(body))
		require.False(t, openAIContinuationStartsCompleteHistory(body))
		require.NoError(t, ValidateOpenAIContinuationInput(body), "unknown native schemas are preserved, not locally rewritten into missing-call errors")
		var items []json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(input), &items))
		require.False(t, openAIContinuationReplayInputProof(body, items, true, true), "a history flag alone cannot prove unknown or reference-only contents")
	}
}
