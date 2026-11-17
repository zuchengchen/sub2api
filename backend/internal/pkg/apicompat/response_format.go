package apicompat

import "encoding/json"

func chatResponseFormatToResponsesTextFormat(raw json.RawMessage) (json.RawMessage, error) {
	raw = normalizedRawJSON(raw)
	if len(raw) == 0 {
		return nil, nil
	}

	obj, ok := uniqueRawObject(raw)
	if !ok {
		return nil, &RequestContractError{Param: "response_format", Message: "response_format must be an unambiguous JSON object"}
	}
	if rawString(obj["type"]) != "json_schema" {
		return raw, nil
	}
	for key := range obj {
		if key != "type" && key != "json_schema" {
			return nil, &RequestContractError{Param: "response_format", Message: "JSON schema response_format fields must be inside json_schema; outer extensions cannot be translated without ambiguity"}
		}
	}

	schemaRaw := normalizedRawJSON(obj["json_schema"])
	if len(schemaRaw) == 0 {
		return raw, nil
	}

	schema, ok := uniqueRawObject(schemaRaw)
	if !ok {
		return nil, &RequestContractError{Param: "response_format.json_schema", Message: "response_format.json_schema must be an unambiguous JSON object"}
	}
	if _, ambiguous := schema["type"]; ambiguous {
		return nil, &RequestContractError{Param: "response_format.json_schema", Message: "response_format.json_schema must not override the output format type"}
	}
	schema["type"] = rawJSONString("json_schema")

	out, err := json.Marshal(schema)
	if err != nil {
		return raw, err
	}
	return out, nil
}

func responsesTextFormatToChatResponseFormat(raw json.RawMessage) json.RawMessage {
	raw = normalizedRawJSON(raw)
	if len(raw) == 0 {
		return nil
	}

	obj, ok := rawJSONObject(raw)
	if !ok || rawString(obj["type"]) != "json_schema" {
		return raw
	}
	if _, alreadyChatShape := obj["json_schema"]; alreadyChatShape {
		return raw
	}

	schema := make(map[string]json.RawMessage, len(obj))
	for key, value := range obj {
		if key == "type" {
			continue
		}
		schema[key] = value
	}
	if len(schema) == 0 {
		return raw
	}

	schemaRaw, err := json.Marshal(schema)
	if err != nil {
		return raw
	}
	out, err := json.Marshal(map[string]json.RawMessage{
		"type":        rawJSONString("json_schema"),
		"json_schema": schemaRaw,
	})
	if err != nil {
		return raw
	}
	return out
}

func normalizedRawJSON(raw json.RawMessage) json.RawMessage {
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func rawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	return obj, true
}

func rawJSONString(value string) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
