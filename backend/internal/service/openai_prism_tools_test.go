package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func prismToolResponse(kind string) map[string]any {
	item := map[string]any{"id": "tool_fixture", "type": kind, "call_id": "call_prism_fixture", "name": "lookup", "namespace": "client", "status": "completed"}
	if kind == "function_call" {
		item["arguments"] = `{"key":"value"}`
	} else {
		item["input"] = "raw\r\n\tcustom input"
	}
	return map[string]any{"id": "resp_fixture", "model": "gpt-6.1-sol", "status": "completed", "usage": nil, "output": []any{item}}
}

func prismToolEvents(t *testing.T, response map[string]any) []map[string]any {
	t.Helper()
	output, ok := response["output"].([]any)
	require.True(t, ok)
	item, ok := output[0].(map[string]any)
	require.True(t, ok)
	added := make(map[string]any)
	for k, v := range item {
		added[k] = v
	}
	field, kind := "arguments", "response.function_call_arguments.done"
	if item["type"] == "custom_tool_call" {
		field, kind = "input", "response.custom_tool_call_input.done"
	}
	added[field], added["status"] = "", "in_progress"
	return []map[string]any{
		{"type": "response.created"},
		{"type": "response.output_item.added", "output_index": 0, "item": added},
		{"type": kind, "output_index": 0, "item_id": item["id"], field: item[field]},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": response},
	}
}

func encodePrismEvents(events []map[string]any) []byte {
	var out strings.Builder
	for i, event := range events {
		event["sequence_number"] = i
		raw, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(&out, "event: %s\ndata: %s\n\n", event["type"], raw)
	}
	return []byte(out.String())
}

func TestPrismClientToolTerminalAndCatalog(t *testing.T) {
	for _, kind := range []string{"function_call", "custom_tool_call"} {
		response := prismToolResponse(kind)
		raw, err := json.Marshal(response)
		require.NoError(t, err)
		id, err := prismBrowserTerminal(raw, "gpt-6.1-sol", false)
		require.NoError(t, err)
		require.Equal(t, "resp_fixture", id)
		sse := encodePrismEvents(prismToolEvents(t, response))
		id, err = prismBrowserTerminal(sse, "gpt-6.1-sol", true)
		require.NoError(t, err)
		require.Equal(t, "resp_fixture", id)
		toolType := "function"
		if kind == "custom_tool_call" {
			toolType = "custom"
		}
		request := []byte(fmt.Sprintf(`{"tools":[{"type":"namespace","name":"client","tools":[{"type":%q,"name":"lookup"}]}]}`, toolType))
		require.NoError(t, prismBrowserValidateToolCatalog(request, raw, false))
		require.NoError(t, prismBrowserValidateToolCatalog(request, sse, true))
		require.Error(t, prismBrowserValidateToolCatalog([]byte(`{"tools":[]}`), sse, true))
		require.Error(t, prismBrowserValidateToolCatalog([]byte(strings.ReplaceAll(string(request), "client", "other")), sse, true))
	}
}

func TestPrismClientToolStreamRejectsConflictingItems(t *testing.T) {
	for _, mutate := range []func([]map[string]any) []map[string]any{
		func(events []map[string]any) []map[string]any {
			events[2]["arguments"] = `{"key":"tampered"}`
			return events
		},
		func(events []map[string]any) []map[string]any { events[2]["item_id"] = "foreign"; return events },
		func(events []map[string]any) []map[string]any { events[1]["output_index"] = -1; return events },
		func(events []map[string]any) []map[string]any { return append(events[:3], events[2:]...) },
	} {
		events := mutate(prismToolEvents(t, prismToolResponse("function_call")))
		_, err := prismBrowserTerminal(encodePrismEvents(events), "gpt-6.1-sol", true)
		require.Error(t, err)
	}
}

func TestPrismCallerIdentityCannotBeSpoofed(t *testing.T) {
	identity := func(key, account int64) string {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.Header.Set("X-Prism-Caller-ID", "external-spoof")
		c.Set("api_key", &APIKey{ID: key})
		return prismBrowserCallerID(c, account)
	}
	require.Len(t, identity(7, 42), 64)
	require.NotEqual(t, identity(7, 42), identity(8, 42))
	require.NotEqual(t, identity(7, 42), identity(7, 43))
	require.Empty(t, identity(0, 42))
}

func TestPrismClientToolGatewayForward(t *testing.T) {
	response := prismToolResponse("function_call")
	sse := encodePrismEvents(prismToolEvents(t, response))
	var caller string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller = r.Header.Get("X-Prism-Caller-ID")
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write(sse)
	}))
	defer server.Close()
	s, account := prismTestService(server.URL)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("X-Prism-Caller-ID", "external-spoof")
	c.Set("api_key", &APIKey{ID: 7})
	body := []byte(`{"model":"gpt-6.1-sol","stream":true,"input":"fixture","tools":[{"type":"namespace","name":"client","tools":[{"type":"function","name":"lookup"}]}]}`)
	result, err := s.forwardPrismBrowser(context.Background(), c, account, body, time.Now())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, prismBrowserCallerID(c, account.ID), caller)
	require.NotEqual(t, "external-spoof", caller)
	require.Greater(t, result.Usage.InputTokens+result.Usage.OutputTokens, 0)
	require.Equal(t, "estimated", w.Header().Get("X-Prism-Usage"))
}
