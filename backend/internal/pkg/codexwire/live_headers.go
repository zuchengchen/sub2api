package codexwire

import "net/http"

// The fixed 0.166.0 native/custom-CA Realtime captures agree on these orders.
// Realtime's request set differs from Responses; do not reuse its template.
var codexLiveHTTPHeaderOrder = []string{
	"version", "openai-alpha", "x-session-id", "session-id", "thread-id",
	"x-codex-turn-metadata", // optional saved source, never synthesized here
	"x-oai-attestation", "authorization", "chatgpt-account-id", "x-openai-fedramp",
	"content-type", "originator", "user-agent", "accept", "cookie",
}

var codexLiveWSHeaderOrder = []string{
	"user-agent", "originator", "chatgpt-account-id", "authorization", "x-openai-fedramp",
	"x-oai-attestation", "version", "openai-alpha", "x-session-id", "session-id", "thread-id",
	"x-codex-turn-metadata", "cookie",
}

// OrderCodexLiveWSHeaders scopes Realtime's handshake to its own header set.
func OrderCodexLiveWSHeaders(headers http.Header) []HeaderKV {
	return orderByTemplate(headers, codexLiveWSHeaderOrder)
}
