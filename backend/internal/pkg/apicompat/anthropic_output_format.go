package apicompat

import (
	"bytes"
	"encoding/json"
)

// AnthropicOutputFormatError reports a format that cannot be translated without
// discarding or weakening an explicit output contract. Messages and parameters
// are fixed strings; schema contents and arbitrary format values stay private.
type AnthropicOutputFormatError struct {
	Param   string
	Message string
}

func (e *AnthropicOutputFormatError) Error() string { return e.Message }

// Anthropic JSON outputs use constrained decoding. Responses requires a name
// and strict:true to express the same contract. Preserve the raw schema and all
// extension fields, so unsupported constraints remain visible to the upstream.
// Never infer types, required fields or additionalProperties here.
//
// Protocol references:
// https://platform.claude.com/docs/en/api/messages/create
// https://platform.claude.com/docs/en/build-with-claude/structured-outputs
// https://developers.openai.com/api/reference/resources/responses/methods/create/
func anthropicOutputFormatToResponsesTextFormat(raw json.RawMessage) (json.RawMessage, error) {
	raw = normalizedRawJSON(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	invalid := func(param, message string) error {
		return &AnthropicOutputFormatError{Param: param, Message: message}
	}
	if !json.Valid(raw) || raw[0] != '{' {
		return nil, invalid("output_config.format", "output_config.format must be a JSON object or null")
	}

	// Decode only the format envelope. RawMessage retains schema numbers and
	// constraints without numeric coercion; duplicate envelope keys are ambiguous.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	_, _ = decoder.Token() // json.Valid and the first byte establish an object.
	format := make(map[string]json.RawMessage)
	for decoder.More() {
		token, _ := decoder.Token()
		key := token.(string)
		if _, exists := format[key]; exists {
			return nil, invalid("output_config.format", "output_config.format must not contain duplicate fields")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, invalid("output_config.format", "output_config.format must be a JSON object or null")
		}
		format[key] = value
	}

	if rawString(format["type"]) != "json_schema" {
		return nil, invalid("output_config.format.type", "This Messages bridge supports output_config.format.type=json_schema; the requested format cannot be translated")
	}
	if _, ambiguous := format["json_schema"]; ambiguous {
		return nil, invalid("output_config.format", "output_config.format must not mix Messages schema and Chat json_schema envelopes")
	}
	schema := bytes.TrimSpace(format["schema"])
	if len(schema) == 0 || schema[0] != '{' {
		return nil, invalid("output_config.format.schema", "output_config.format.schema must be a JSON object")
	}
	if strict, exists := format["strict"]; exists && !bytes.Equal(bytes.TrimSpace(strict), []byte("true")) {
		return nil, invalid("output_config.format.strict", "Messages JSON output requires strict schema adherence; output_config.format.strict cannot disable it")
	}
	if _, exists := format["name"]; !exists {
		format["name"] = json.RawMessage(`"response"`)
	}
	format["strict"] = json.RawMessage("true")
	return json.Marshal(format)
}
