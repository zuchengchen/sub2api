package apicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// RequestContractError rejects a conversion that would lose an explicit
// contract. Messages and parameter paths never contain user-supplied values.
type RequestContractError struct {
	Param   string
	Message string
}

func (e *RequestContractError) Error() string { return e.Message }

func uniqueRawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	_, _ = decoder.Token()
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		token, _ := decoder.Token()
		key := token.(string)
		if _, duplicate := object[key]; duplicate {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		object[key] = value
	}
	return object, true
}

func chatCustomToolToResponses(tool ChatTool, index int) (ResponsesTool, error) {
	param := fmt.Sprintf("tools[%d].custom", index)
	invalid := func(path string) (ResponsesTool, error) {
		return ResponsesTool{}, &RequestContractError{Param: path, Message: "The custom tool declaration cannot be translated without losing its input contract"}
	}
	custom, ok := uniqueRawObject(tool.Custom)
	if !ok || rawString(custom["name"]) == "" {
		return invalid(param)
	}
	if _, ambiguous := custom["type"]; ambiguous {
		return invalid(param)
	}
	if rawFormat := normalizedRawJSON(custom["format"]); len(rawFormat) > 0 {
		format, valid := uniqueRawObject(rawFormat)
		if !valid {
			return invalid(param + ".format")
		}
		if rawString(format["type"]) == "grammar" {
			grammar, valid := uniqueRawObject(format["grammar"])
			if !valid || rawString(grammar["syntax"]) == "" || len(grammar["definition"]) == 0 {
				return invalid(param + ".format.grammar")
			}
			for key, value := range grammar {
				if _, ambiguous := format[key]; ambiguous {
					return invalid(param + ".format")
				}
				format[key] = value
			}
			delete(format, "grammar")
			custom["format"], _ = json.Marshal(format)
		}
	}
	// Only the documented wrapper changes. Unknown fields remain visible for
	// upstream validation, and collisions fail rather than selecting a winner.
	if len(tool.rawDeclaration) > 0 {
		outer, valid := uniqueRawObject(tool.rawDeclaration)
		if !valid {
			return invalid(param)
		}
		for key, value := range outer {
			if key == "type" || key == "custom" {
				continue
			}
			if _, ambiguous := custom[key]; ambiguous {
				return invalid(param)
			}
			custom[key] = value
		}
	}
	custom["type"] = json.RawMessage(`"custom"`)
	raw, err := json.Marshal(custom)
	if err != nil {
		return invalid(param)
	}
	return ResponsesTool{Type: "custom", Name: rawString(custom["name"]), Description: rawString(custom["description"]), rawDeclaration: raw}, nil
}
