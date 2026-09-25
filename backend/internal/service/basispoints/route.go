package basispoints

import (
	"strings"

	"github.com/tidwall/gjson"
)

// NativeFallbackReason reports capabilities that must stay on the native Codex
// channel because Basispoints cannot execute them. Empty means the request can
// use the BPS bridge.
func NativeFallbackReason(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		fallback := ""
		tools.ForEach(func(_, tool gjson.Result) bool {
			kind := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
			switch kind {
			case "image_generation":
				fallback = "image_generation"
				return false
			case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26":
				if tool.Get("external_web_access").Bool() || tool.Get("search_context_size").String() == "high" {
					fallback = "web_search"
					return false
				}
			}
			return true
		})
		if fallback != "" {
			return fallback
		}
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if choice.Exists() && choice.Type == gjson.JSON {
		name := choice.Get("name").String()
		if strings.Contains(strings.ToLower(name), "web_search") || strings.Contains(strings.ToLower(name), "image_generation") {
			return "tool_choice"
		}
	}
	// Inline data images are handled by Sub2API's local relay before Prepare;
	// leave them on BPS so the relay can rewrite them to signed HTTPS URLs.
	return ""
}
