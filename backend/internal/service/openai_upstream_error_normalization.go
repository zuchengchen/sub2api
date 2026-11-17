package service

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These are parsing-work limits, not upstream request-size limits. Exceeding
// them only declines classification/automatic repair; it never truncates input.
const (
	maxOpenAIErrorNormalizationBytes = 1 << 20
	maxOpenAIErrorJSONDepth          = 64
	maxOpenAIErrorJSONFields         = 65536
)

type openAIUpstreamError struct {
	Type       string
	Code       string
	Param      string
	Message    string
	StatusCode int
	// ErrorBody preserves the error object's fields inside the HTTP envelope.
	// It is unsanitized, like the input; callers must keep existing redaction.
	ErrorBody []byte
}

// normalizeOpenAIUpstreamError reads HTTP errors, bare WS errors and failed
// Responses events through one contract. It never interprets prose as nested
// JSON, combines separate error objects, or chooses among duplicate keys.
func normalizeOpenAIUpstreamError(payload []byte) (openAIUpstreamError, bool) {
	if len(payload) == 0 || len(payload) > maxOpenAIErrorNormalizationBytes || !openAIUnambiguousJSONObject(payload) {
		return openAIUpstreamError{}, false
	}
	root := parseRawJSONView(payload)
	errObject := root.Get("error")
	response := root.Get("response")
	nestedError := response.Get("error")
	if errObject.Exists() && nestedError.Exists() {
		return openAIUpstreamError{}, false
	}
	if nestedError.Exists() {
		if status := response.Get("status"); status.Exists() && (status.Type != gjson.String || status.String() != "failed") {
			return openAIUpstreamError{}, false
		}
		errObject = nestedError
	}
	bare := !errObject.Exists()
	if bare {
		// An event's type=error is a discriminator, not the provider error type.
		typ := root.Get("type")
		if typ.Type != gjson.String || (typ.String() != "error" && !strings.HasSuffix(typ.String(), "_error")) ||
			(!root.Get("message").Exists() && !root.Get("code").Exists()) || response.Exists() {
			return openAIUpstreamError{}, false
		}
		errObject = root
	}
	if !errObject.IsObject() {
		return openAIUpstreamError{}, false
	}
	if !bare {
		if typ := root.Get("type"); typ.Exists() && (typ.Type != gjson.String ||
			(typ.String() != "error" && typ.String() != "response.failed")) {
			return openAIUpstreamError{}, false
		}
		// Do not let an outer field contradict the selected structured error.
		for _, key := range []string{"code", "param", "message"} {
			outer := root.Get(key)
			if outer.Exists() && outer.Type != gjson.Null && outer.Raw != errObject.Get(key).Raw {
				if outer.Type != gjson.String || errObject.Get(key).Type != gjson.String || outer.String() != errObject.Get(key).String() {
					return openAIUpstreamError{}, false
				}
			}
		}
	}
	var result openAIUpstreamError
	for key, target := range map[string]*string{
		"type": &result.Type, "code": &result.Code, "param": &result.Param, "message": &result.Message,
	} {
		value := errObject.Get(key)
		if value.Exists() && value.Type != gjson.Null {
			if value.Type != gjson.String {
				return openAIUpstreamError{}, false
			}
			*target = value.String()
		}
	}
	if bare && result.Type == "error" {
		result.Type = ""
	}
	for _, object := range []gjson.Result{errObject, root} {
		for _, key := range []string{"status_code", "status"} {
			value := object.Get(key)
			if !value.Exists() || value.Type == gjson.Null {
				continue
			}
			// A Response object may also carry status="failed". This isn't an
			// HTTP error status and is only permitted on the envelope.
			if object.Raw == root.Raw && key == "status" && value.Type == gjson.String && value.String() == "failed" && !bare {
				continue
			}
			status, err := strconv.Atoi(value.String())
			if (value.Type != gjson.Number && value.Type != gjson.String) || err != nil || status < 400 || status > 599 ||
				(result.StatusCode != 0 && result.StatusCode != status) {
				return openAIUpstreamError{}, false
			}
			result.StatusCode = status
		}
	}
	objectBody := []byte(errObject.Raw)
	if bare && errObject.Get("type").String() == "error" {
		var err error
		objectBody, err = sjson.DeleteBytes(objectBody, "type")
		if err != nil {
			return openAIUpstreamError{}, false
		}
	}
	if result.StatusCode != 0 && !errObject.Get("status_code").Exists() && !errObject.Get("status").Exists() {
		var err error
		objectBody, err = sjson.SetBytes(objectBody, "status_code", result.StatusCode)
		if err != nil {
			return openAIUpstreamError{}, false
		}
	}
	result.ErrorBody = append([]byte(`{"error":`), objectBody...)
	result.ErrorBody = append(result.ErrorBody, '}')
	return result, true
}

// openAIUnambiguousJSONObject limits nesting before invoking GJSON's recursive
// validator, then checks decoded keys at every depth. Values remain raw views,
// preserving large integers, tool JSON strings, images and opaque history.
func openAIUnambiguousJSONObject(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	depth := 0
	inString, escaped := false, false
	for _, ch := range body {
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxOpenAIErrorJSONDepth {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	if depth != 0 || inString || !gjson.ValidBytes(body) {
		return false
	}
	root := parseRawJSONView(body)
	if !root.IsObject() {
		return false
	}
	remaining := maxOpenAIErrorJSONFields
	return openAIJSONUniqueKeys(root, &remaining)
}

func openAIJSONUniqueKeys(value gjson.Result, remaining *int) bool {
	if !value.IsObject() && !value.IsArray() {
		return true
	}
	var keys map[string]struct{}
	if value.IsObject() {
		keys = make(map[string]struct{})
	}
	valid := true
	value.ForEach(func(key, child gjson.Result) bool {
		*remaining -= 1
		if *remaining < 0 {
			valid = false
			return false
		}
		if keys != nil {
			if _, duplicate := keys[key.Str]; duplicate {
				valid = false
				return false
			}
			keys[key.Str] = struct{}{}
		}
		valid = openAIJSONUniqueKeys(child, remaining)
		return valid
	})
	return valid
}
