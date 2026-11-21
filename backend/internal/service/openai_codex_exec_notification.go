package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
)

// These IDs identify wire items a compatibility transform must preserve. An
// output-only delta can refer to an exec on the original connection. This set
// authorizes neither the continuation nor a retry.
func codexExecNotificationIDs(input gjson.Result) map[string]bool {
	ids, _ := collectCodexExecNotificationIDs(input)
	return ids
}

// A malformed named output cannot identify a pending call on a WebSocket.
// Report it during the same scan as the valid IDs.
func collectCodexExecNotificationIDs(input gjson.Result) (ids map[string]bool, malformedID bool) {
	collect := func(item gjson.Result) {
		if !hasCodexExecNotificationPayload(item) {
			return
		}
		id := item.Get("call_id")
		if !openAIContinuationProofString(id, true) {
			malformedID = true
			return
		}
		if ids == nil {
			ids = make(map[string]bool)
		}
		ids[id.Str] = true
	}
	if !input.IsArray() {
		if input.IsObject() {
			collect(input)
		}
		return ids, malformedID
	}
	input.ForEach(func(_, item gjson.Result) bool {
		collect(item)
		return true
	})
	if len(ids) > 0 {
		// A different tool (including exec in a private namespace) may have
		// supplied an ordinary named result. Exclude it only if every known
		// call with that ID is a different tool.
		knownExec := make(map[string]bool)
		input.ForEach(func(_, item gjson.Result) bool {
			id := item.Get("call_id").Str
			if ids[id] && openAIContinuationProofToolDirection(item.Get("type").Str) > 0 {
				knownExec[id] = knownExec[id] || isCodexExecNotificationCall(item)
			}
			return true
		})
		for id, isExec := range knownExec {
			if !isExec {
				delete(ids, id)
			}
		}
	}
	return ids, malformedID
}

func codexExecNotificationConversionError(body []byte) error {
	if len(codexExecNotificationIDs(parseRawJSONView(body).Get("input"))) == 0 {
		return nil
	}
	return &apicompat.RequestContractError{
		Param: "input", Message: "This upstream protocol cannot preserve Codex exec notifications. Use a native Responses route.",
	}
}
