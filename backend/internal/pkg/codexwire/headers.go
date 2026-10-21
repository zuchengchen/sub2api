// Package codexwire implements byte-exact wire transports for the OpenAI Codex
// lane: a raw WebSocket upgrade handshake plus frame codec, and a raw HTTP/1.1
// client, so the gateway can reproduce the official Codex client's wire format
// (header order, casing, extension negotiation) that Go's net/http stack
// canonicalizes away.
package codexwire

import (
	"net/http"
	"strings"
)

// HeaderKV is one header line in wire order. Name carries the exact casing to
// write; Value is written verbatim.
type HeaderKV struct {
	Name  string
	Value string
}

// codexWSHeaderOrder is the official Codex client's WebSocket upgrade header
// order after the five tungstenite-generated lines, captured verbatim from a
// real client handshake (codex-rs HeaderMap iteration order for this key set).
// Optional headers are slotted at their documented insertion positions:
//   - x-codex-parent-thread-id, x-openai-subagent follow x-codex-turn-metadata
//     (compatibility_headers emits them after the metadata block).
//   - x-oai-attestation follows x-codex-routing-hint
//     (build_websocket_headers inserts it right after the routing hint).
//   - x-responsesapi-include-timing-metrics and x-openai-memgen-request ride in
//     the same codex block; absent from captures they are placed adjacent to
//     their sibling keys per codex-rs insertion order.
//   - cookie is merged lazily by the websocket client before dialing.
var codexWSHeaderOrder = []string{
	"chatgpt-account-id",
	"authorization",
	"user-agent",
	"originator",
	"openai-beta",
	"version",
	"x-codex-beta-features",
	"x-client-request-id",
	"session-id",
	"thread-id",
	"x-codex-window-id",
	"x-codex-turn-metadata",
	"x-codex-parent-thread-id",
	"x-openai-subagent",
	"x-openai-memgen-request",
	"x-responsesapi-include-timing-metrics",
	"x-codex-routing-hint",
	"x-oai-attestation",
	"cookie",
}

// OrderCodexWSHeaders flattens h into the official Codex WS handshake order.
// It returns (ordered, leftovers): headers matching the official set in wire
// order, plus every remaining header verbatim so the caller can relocate or
// drop them deliberately (e.g. x-codex-turn-state belongs in client_metadata
// on the wire, not the handshake).
func OrderCodexWSHeaders(h http.Header) []HeaderKV {
	ordered, _ := SplitCodexWSHeaders(h)
	return ordered
}

// SplitCodexWSHeaders partitions h into the officially-ordered Codex set and
// all remaining headers (preserving map iteration order for the remainder).
func SplitCodexWSHeaders(h http.Header) (ordered, leftover []HeaderKV) {
	if len(h) == 0 {
		return nil, nil
	}
	matched := make(map[string]bool, len(h))
	out := make([]HeaderKV, 0, len(h)+4)
	for _, name := range codexWSHeaderOrder {
		for key, values := range h {
			if !strings.EqualFold(key, name) {
				continue
			}
			matched[key] = true
			for _, value := range values {
				if strings.TrimSpace(value) == "" {
					continue
				}
				out = append(out, HeaderKV{Name: name, Value: value})
			}
		}
	}
	for key, values := range h {
		if matched[key] {
			continue
		}
		// http::HeaderMap lowercases every name on iteration — Go's canonical
		// Title-Case keys would differ on the wire.
		name := strings.ToLower(key)
		for _, value := range values {
			leftover = append(leftover, HeaderKV{Name: name, Value: value})
		}
	}
	return out, leftover
}
