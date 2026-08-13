package service

import "strings"

// edgeChallengeBodyScanMaxBytes bounds the prefix scanned when classifying a 403
// body. Challenge markers live in the opening <head>, so a short prefix is
// enough and keeps a multi-megabyte body from being lowercased on the hot path.
const edgeChallengeBodyScanMaxBytes = 8 << 10

// openAIEdgeChallengeMarkers are substrings that only appear in edge/anti-bot
// interstitials (Cloudflare's challenge page, or the ChatGPT HTML holding page
// that carries a <meta http-equiv="refresh">).
var openAIEdgeChallengeMarkers = []string{
	"just a moment",
	"cf-browser-verification",
	"cf_chl_",
	"cf-challenge",
	"cdn-cgi/challenge-platform",
	"enable javascript and cookies to continue",
	"checking your browser",
	"attention required",
	"cloudflare",
}

// isOpenAIEdgeChallenge403 reports whether a 403 body is an edge/anti-bot
// challenge page rather than an account-level rejection.
//
// An edge challenge is scoped to the egress IP and TLS fingerprint: every
// account behind the same IP is challenged at once. Counting those against
// individual accounts turns one client request into a group-wide outage —
// each failover hop collects another 10-minute temp-unschedulable block.
//
// Upstream account errors are always JSON; challenges are always HTML or
// bare text. The JSON check runs first so a hypothetical JSON error that
// merely mentions "cloudflare" is still treated as account-level.
func isOpenAIEdgeChallenge403(responseBody []byte) bool {
	body := responseBody
	if len(body) > edgeChallengeBodyScanMaxBytes {
		body = body[:edgeChallengeBodyScanMaxBytes]
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)

	if strings.HasPrefix(lower, "{") || strings.HasPrefix(lower, "[") {
		return false
	}

	isHTML := strings.HasPrefix(lower, "<html") ||
		strings.HasPrefix(lower, "<!doctype html") ||
		strings.HasPrefix(lower, "<head")
	if !isHTML {
		return matchesOpenAIEdgeChallengeMarker(lower)
	}

	if strings.Contains(lower, "http-equiv=\"refresh\"") ||
		strings.Contains(lower, "http-equiv='refresh'") ||
		strings.Contains(lower, "http-equiv=refresh") {
		return true
	}

	return matchesOpenAIEdgeChallengeMarker(lower)
}

func matchesOpenAIEdgeChallengeMarker(lowerBody string) bool {
	for _, marker := range openAIEdgeChallengeMarkers {
		if strings.Contains(lowerBody, marker) {
			return true
		}
	}
	return false
}
