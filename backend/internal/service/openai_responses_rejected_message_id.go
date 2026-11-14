package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Match the whole validation message, retaining the ID's case. A mention of an
// ID in an unrelated upstream error is not permission to rewrite the request.
var openAIResponsesRejectedMessageIDPattern = regexp.MustCompile(`(?i)^invalid\s+['"](input\[(\d+)\]\.id)['"]:\s*['"]([^'"\r\n]+)['"]\.\s*expected an id that begins with ['"]msg['"]\.?$`)

func openAIResponsesRejectedMessageID(responseBody []byte) (int, string, bool) {
	if !gjson.ValidBytes(responseBody) {
		return 0, "", false
	}
	root := parseRawJSONView(responseBody)
	errorObject := root.Get("error")
	if !root.IsObject() || hasDuplicateJSONObjectKeys(root) || !errorObject.IsObject() || hasDuplicateJSONObjectKeys(errorObject) {
		return 0, "", false
	}
	if strings.TrimSpace(errorObject.Get("code").String()) != "invalid_value" {
		return 0, "", false
	}
	errType := errorObject.Get("type")
	if errType.Exists() && errType.String() != "invalid_request_error" {
		return 0, "", false
	}
	match := openAIResponsesRejectedMessageIDPattern.FindStringSubmatch(strings.TrimSpace(extractUpstreamErrorMessage(responseBody)))
	if len(match) != 4 || errorObject.Get("param").String() != match[1] {
		return 0, "", false
	}
	index, err := strconv.Atoi(match[2])
	if err != nil || index < 0 || strings.HasPrefix(match[3], "msg") {
		return 0, "", false
	}
	return index, match[3], true
}

// A complete inline message can be replayed without its optional object ID.
// Only repair the exact ID the provider rejected; native IDs on successful
// requests, opaque reasoning, tool call identities and references stay intact.
func removeOpenAIResponsesRejectedMessageID(body, responseBody []byte) ([]byte, string, bool, error) {
	index, rejectedID, matched := openAIResponsesRejectedMessageID(responseBody)
	if !matched || !gjson.ValidBytes(body) {
		return nil, "", false, nil
	}
	root := parseRawJSONView(body)
	if !root.IsObject() || hasDuplicateJSONObjectKeys(root) {
		return nil, "", false, nil
	}
	input := root.Get("input")
	if !input.IsArray() {
		return nil, "", false, nil
	}
	itemPath := fmt.Sprintf("input.%d", index)
	item := root.Get(itemPath)
	if !item.IsObject() || hasDuplicateJSONObjectKeys(item) {
		return nil, "", false, nil
	}
	id := item.Get("id")
	if id.Type != gjson.String || id.String() != rejectedID {
		return nil, "", false, nil
	}
	itemType := item.Get("type")
	if itemType.Exists() && (itemType.Type != gjson.String || itemType.String() != "message") {
		return nil, "", false, nil
	}
	switch item.Get("role").String() {
	case "assistant", "user", "system", "developer":
	default:
		return nil, "", false, nil
	}
	content := item.Get("content")
	if content.Type != gjson.String && !content.IsArray() {
		return nil, "", false, nil
	}
	// Duplicate structural keys can hide a reference from GJSON while the
	// upstream decoder sees the last occurrence. Decline ambiguous input items.
	unsafeReference := false
	input.ForEach(func(_, candidate gjson.Result) bool {
		if candidate.IsObject() && (hasDuplicateJSONObjectKeys(candidate) ||
			(candidate.Get("type").String() == "item_reference" && candidate.Get("id").String() == rejectedID)) {
			unsafeReference = true
			return false
		}
		return true
	})
	if unsafeReference {
		return nil, "", false, nil
	}
	retryBody, err := sjson.DeleteBytes(body, itemPath+".id")
	if err != nil {
		return nil, "", false, fmt.Errorf("delete rejected message ID at input[%d]: %w", index, err)
	}
	return retryBody, fmt.Sprintf("message ID prefix rejection at input[%d].id", index), true, nil
}
