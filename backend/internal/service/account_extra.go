package service

import (
	"encoding/json"
	"maps"
)

func cloneExtraJSON(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	raw, err := json.Marshal(extra)
	if err != nil {
		return maps.Clone(extra)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return maps.Clone(extra)
	}
	return out
}
