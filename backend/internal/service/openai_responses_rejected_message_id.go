package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Match the whole validation message, retaining the ID's case. A mention of an
// ID in an unrelated upstream error is not permission to rewrite the request.
var openAIResponsesRejectedMessageIDPattern = regexp.MustCompile(`(?i)^invalid\s+(['"])(input\[(\d+)\]\.id)(['"]):\s*(['"])([^'"\r\n]+)(['"])\.\s*expected an id that begins with (['"])msg(['"])\.?$`)

// This exact prefix is emitted by the provider's ApiIdParam validator. It is
// evidence of the rejected field even when its structured code/param are absent.
var openAIResponsesRejectedMessageIDPrefixPattern = regexp.MustCompile(`^\[ApiIdParam\] \[(input\[[0-9]+\]\.id)\] \[invalid_id_prefix\] (.+)$`)

const maxOpenAIRejectedMessageIDLength = 1024

func openAIResponsesRejectedMessageID(responseBody []byte) (int, string, bool) {
	upstreamError, ok := normalizeOpenAIUpstreamError(responseBody)
	if !ok {
		return 0, "", false
	}
	return openAIResponsesRejectedMessageIDFromError(upstreamError)
}

func openAIResponsesRejectedMessageIDFromError(upstreamError openAIUpstreamError) (int, string, bool) {
	if upstreamError.StatusCode != 0 && upstreamError.StatusCode != 400 {
		return 0, "", false
	}
	if upstreamError.Type != "" && upstreamError.Type != "invalid_request_error" {
		return 0, "", false
	}
	message := strings.TrimSpace(upstreamError.Message)
	if len(message) > maxOpenAIRejectedMessageIDLength+512 || strings.ContainsAny(message, "\r\n") {
		return 0, "", false
	}
	prefixParam := ""
	if prefix := openAIResponsesRejectedMessageIDPrefixPattern.FindStringSubmatch(message); len(prefix) == 3 {
		prefixParam, message = prefix[1], prefix[2]
	}
	switch upstreamError.Code {
	case "invalid_value", "invalid_id_prefix":
	case "":
		if prefixParam == "" {
			return 0, "", false
		}
	default:
		return 0, "", false
	}
	match := openAIResponsesRejectedMessageIDPattern.FindStringSubmatch(message)
	if len(match) != 10 || match[1] != match[4] || match[5] != match[7] || match[8] != match[9] ||
		(prefixParam != "" && prefixParam != match[2]) ||
		(upstreamError.Param != "" && upstreamError.Param != match[2]) ||
		(upstreamError.Param == "" && prefixParam == "") {
		return 0, "", false
	}
	index, err := strconv.Atoi(match[3])
	rejectedID := match[6]
	if err != nil || index < 0 || match[2] != fmt.Sprintf("input[%d].id", index) || len(rejectedID) > maxOpenAIRejectedMessageIDLength ||
		strings.IndexFunc(rejectedID, unicode.IsControl) >= 0 || strings.HasPrefix(rejectedID, "msg") {
		return 0, "", false
	}
	for _, protectedPrefix := range []string{"rs_", "fc_", "call_"} {
		if strings.HasPrefix(rejectedID, protectedPrefix) {
			return 0, "", false
		}
	}
	return index, rejectedID, true
}

// A complete inline message can be replayed without its optional object ID.
// Only repair the exact ID the provider rejected; native IDs on successful
// requests, opaque reasoning, tool call identities and references stay intact.
func removeOpenAIResponsesRejectedMessageID(body, responseBody []byte) ([]byte, string, bool, error) {
	index, rejectedID, matched := openAIResponsesRejectedMessageID(responseBody)
	if !matched || !openAIUnambiguousJSONObject(body) {
		return nil, "", false, nil
	}
	root := parseRawJSONView(body)
	input := root.Get("input")
	if !input.IsArray() {
		return nil, "", false, nil
	}
	itemPath := fmt.Sprintf("input.%d", index)
	item := root.Get(itemPath)
	if !item.IsObject() {
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
	// Incomplete/contradictory call or reasoning items must not be repaired by
	// treating their identities as optional message IDs.
	for _, key := range []string{"call_id", "arguments", "output", "encrypted_content", "summary"} {
		if item.Get(key).Exists() {
			return nil, "", false, nil
		}
	}
	switch item.Get("role").String() {
	case "assistant", "user", "system", "developer":
	default:
		return nil, "", false, nil
	}
	content := item.Get("content")
	if !openAIResponsesCompleteInlineMessageContent(content) {
		return nil, "", false, nil
	}
	// A second occurrence could be a reference or a duplicate identity. Both
	// make removal unsafe, even if the other item's type is currently unknown.
	unsafeReference := false
	input.ForEach(func(key, candidate gjson.Result) bool {
		if int(key.Int()) != index && candidate.IsObject() && candidate.Get("id").String() == rejectedID {
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

func openAIResponsesCompleteInlineMessageContent(content gjson.Result) bool {
	if content.Type == gjson.String {
		return true
	}
	if !content.IsArray() {
		return false
	}
	complete := true
	content.ForEach(func(_, part gjson.Result) bool {
		if !part.IsObject() {
			complete = false
			return false
		}
		switch part.Get("type").String() {
		case "input_text", "output_text":
			complete = part.Get("text").Type == gjson.String
		case "refusal":
			complete = part.Get("refusal").Type == gjson.String
		case "input_image":
			complete = part.Get("image_url").Type == gjson.String || part.Get("file_id").Type == gjson.String
		case "input_file":
			complete = part.Get("file_data").Type == gjson.String || part.Get("file_id").Type == gjson.String || part.Get("file_url").Type == gjson.String
		case "input_audio":
			complete = part.Get("input_audio.data").Type == gjson.String && part.Get("input_audio.format").Type == gjson.String
		default:
			complete = false
		}
		return complete
	})
	return complete
}
