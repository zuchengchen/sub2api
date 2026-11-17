package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityContinuationIndexedOutputIntegrity(t *testing.T) {
	item := `{"type":"message","id":"msg_kept","role":"assistant","content":"kept"}`
	for _, index := range []string{`-1`, `1.5`, `"0"`, `null`, `8192`, `1e100`} {
		t.Run(index, func(t *testing.T) {
			collector := &openAIWSToolCallReplayCollector{}
			collector.AddEvent("response.output_item.done", []byte(`{"type":"response.output_item.done","output_index":`+index+`,"item":`+item+`}`))
			collector.AddEvent("response.completed", []byte(`{"type":"response.completed","response":{"output":[`+item+`]}}`))
			require.False(t, collector.Complete())
		})
	}
	t.Run("identical_duplicate", func(t *testing.T) {
		collector := &openAIWSToolCallReplayCollector{}
		event := []byte(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`)
		collector.AddEvent("response.output_item.done", event)
		collector.AddEvent("response.output_item.done", event)
		collector.AddEvent("response.completed", []byte(`{"type":"response.completed","response":{"status":"completed"}}`))
		require.True(t, collector.Complete())
		require.Len(t, collector.AllItems(), 1)
		require.JSONEq(t, item, string(collector.AllItems()[0]))
	})
	t.Run("duplicate_index_field", func(t *testing.T) {
		collector := &openAIWSToolCallReplayCollector{}
		collector.AddEvent("response.output_item.done", []byte(`{"type":"response.output_item.done","output_index":0,"output_index":1,"item":`+item+`}`))
		collector.AddEvent("response.completed", []byte(`{"type":"response.completed","response":{"output":[`+item+`]}}`))
		require.False(t, collector.Complete())
	})
	t.Run("canonical_array_provides_missing_index", func(t *testing.T) {
		collector := &openAIWSToolCallReplayCollector{}
		collector.AddEvent("response.output_item.done", []byte(`{"type":"response.output_item.done","item":`+item+`}`))
		collector.AddEvent("response.completed", []byte(`{"type":"response.completed","response":{"output":[`+item+`]}}`))
		require.True(t, collector.Complete(), "the full terminal array independently establishes exact order")
	})
}

func TestOAuthReliabilityContinuationUnfamiliarCallIsNative(t *testing.T) {
	for _, call := range []string{
		`{"type":"computer_call","call_id":"c","action":{"type":"move","x":10,"y":20}}`,
		`{"type":"function_call","call_id":"c","name":"lookup","status":"future_status","arguments":"{}"}`,
		`{"type":"mcp_tool_call","call_id":"c","name":"lookup","arguments":"{}"}`,
	} {
		output := `{"type":"function_call_output","call_id":"c","output":"preserve"}`
		if gjson.Get(call, "type").String() == "computer_call" {
			output = `{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","image_url":"synthetic"}}`
		}
		body := []byte(`{"model":"gpt-5.5","stream":false,"input":[` + call + `,` + output + `]}`)
		proof := AnalyzeOpenAIContinuationReplayProof(body)
		require.Equal(t, OpenAIContinuationProofUnproven, proof.Verdict)
		require.False(t, proof.ToolContextUnproven)
		require.NoError(t, ValidateOpenAIContinuationInput(body))
		upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(200, `{"id":"resp_native_schema","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)}
		result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), newOpenAIRejectedFieldTestAccount(), body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Len(t, upstream.bodies, 1)
		require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(upstream.lastBody, "input").Raw)
	}
}

func oauthReliabilityContinuationChatResponse(id string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + `{"type":"response.completed","response":{"id":"` + id + `","model":"gpt-5.5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"))}
}

func TestOAuthReliabilityContinuationChatNativeOwnerRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			account := newOpenAIRejectedFieldTestAccount()
			upstream := &httpUpstreamRecorder{responses: []*http.Response{oauthReliabilityContinuationChatResponse("resp_chat_root"), oauthReliabilityContinuationChatResponse("resp_chat_next")}}
			svc := newOpenAIRejectedFieldTestService(upstream)
			svc.toolCorrector = NewCodexToolCorrector()
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"input":"root"}`, stream))
			c := responseAuthOwnerContext(101, 201)
			SetOpenAIHTTPResponseOwner(c, 101, 201)
			result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "resp_chat_root", result.ResponseID)
			next := []byte(fmt.Sprintf(`{"model":"gpt-5.5","stream":%t,"previous_response_id":"resp_chat_root","input":"delta"}`, stream))
			c = responseAuthOwnerContext(101, 202)
			result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, next, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "resp_chat_root", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
			require.Len(t, upstream.bodies, 2)
			current := snapshotOAuthRefreshAccount(account)
			current.Credentials["api_key"] = "synthetic-rotated-key"
			result, err = svc.ForwardAsChatCompletions(context.Background(), responseAuthOwnerContext(101, 201), current, next, "", "")
			require.ErrorIs(t, err, errOpenAIResponseAuthOwnerMismatch)
			require.Nil(t, result)
			require.Len(t, upstream.bodies, 2)
		})
	}
}

func TestOAuthReliabilityContinuationChatCannotConvertAnchors(t *testing.T) {
	for _, anchor := range []string{`"previous_response_id":"resp_native"`, `"conversation":"conv_native"`} {
		account := newOpenAIRejectedFieldTestAccount()
		account.Extra[openai_compat.ExtraKeyResponsesMode] = string(openai_compat.ResponsesSupportModeForceChatCompletions)
		body := []byte(`{"model":"gpt-5.5","stream":false,` + anchor + `,"input":"delta"}`)
		upstream := &httpUpstreamRecorder{}
		c := newOpenAIRejectedFieldTestContext(body)
		result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
		var contextErr *OpenAIContinuationContextError
		require.ErrorAs(t, err, &contextErr)
		require.Nil(t, result)
		require.Empty(t, upstream.bodies)
		require.Equal(t, http.StatusBadRequest, c.Writer.Status())
	}
}

func TestOAuthReliabilityContinuationConversationNeverProvesReplay(t *testing.T) {
	input := []json.RawMessage{json.RawMessage(`{"role":"user","content":"partial root"}`)}
	for _, conversation := range []string{`"conv_native"`, `{"id":"conv_native"}`, `{"id":4}`, `42`} {
		body := []byte(`{"conversation":` + conversation + `,"input":"delta"}`)
		require.False(t, OpenAIContinuationMaySwitchAccount(body))
		require.False(t, openAIContinuationStartsCompleteHistory(body))
		require.False(t, openAIContinuationReplayInputProof(body, input, true, true))
	}
}
