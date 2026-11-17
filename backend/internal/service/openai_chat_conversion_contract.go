package service

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func writeOpenAIChatConversionContractError(c *gin.Context, err error) bool {
	var contractErr *apicompat.RequestContractError
	if !errors.As(err, &contractErr) {
		return false
	}
	setOpsUpstreamError(c, http.StatusBadRequest, contractErr.Message, "")
	writeOpenAIResponsesErrorJSON(c, http.StatusBadRequest, gin.H{"error": gin.H{
		"type": "invalid_request_error", "code": "unsupported_parameter",
		"param": contractErr.Param, "message": contractErr.Message,
	}})
	return true
}

// Even the Responses-shaped compatibility input on /chat/completions receives
// Chat output. Reject known call/result families with no Chat output carrier
// before execution. Hosted tools whose results are incorporated into final text
// are different; do not infer client execution from an unknown environment.
// Opaque arguments and historical results remain untouched.
func validateOpenAIChatResponseToolContract(body []byte) error {
	type list struct {
		tools      gjson.Result
		param      string
		namespaced bool
	}
	root := gjson.ParseBytes(body)
	pending := []list{{tools: root.Get("tools"), param: "tools"}}
	root.Get("input").ForEach(func(key, item gjson.Result) bool {
		if kind := item.Get("type").String(); kind == "additional_tools" || kind == "tool_search_output" {
			pending = append(pending, list{tools: item.Get("tools"), param: fmt.Sprintf("input[%d].tools", key.Int())})
		}
		return true
	})
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if !current.tools.IsArray() {
			continue
		}
		for index, tool := range current.tools.Array() {
			param := fmt.Sprintf("%s[%d]", current.param, index)
			if len(param) > 240 {
				param = "tools"
			}
			unsupported := false
			switch tool.Get("type").String() {
			case "tool_search":
				return &apicompat.RequestContractError{Param: param + ".type", Message: "The Chat Completions response format cannot represent tool_search calls; use the Responses endpoint to preserve their execution contract"}
			case "apply_patch", "computer", "computer_use_preview", "local_shell", "image_generation":
				unsupported = true
			case "shell":
				unsupported = tool.Get("environment.type").String() == "local"
			case "custom":
				if tool.Get("async").Type == gjson.True {
					return &apicompat.RequestContractError{Param: param + ".async", Message: "The Chat Completions response format cannot preserve asynchronous custom tool execution; use the Responses endpoint"}
				}
				if callers := tool.Get("allowed_callers"); callers.IsArray() {
					for _, caller := range callers.Array() {
						if caller.Type == gjson.String && caller.String() == "programmatic" {
							return &apicompat.RequestContractError{Param: param + ".allowed_callers", Message: "The Chat Completions response format cannot preserve programmatic custom tool caller identity; use the Responses endpoint"}
						}
					}
				}
				if current.namespaced {
					return &apicompat.RequestContractError{Param: param + ".type", Message: "The Chat Completions response format cannot preserve a namespaced custom tool call identity; use the Responses endpoint"}
				}
			case "namespace":
				pending = append(pending, list{tools: tool.Get("tools"), param: param + ".tools", namespaced: true}, list{tools: tool.Get("children"), param: param + ".children", namespaced: true})
			}
			if unsupported {
				return &apicompat.RequestContractError{Param: param + ".type", Message: "The Chat Completions response format cannot preserve this tool's output; use the Responses endpoint"}
			}
		}
	}
	return nil
}
