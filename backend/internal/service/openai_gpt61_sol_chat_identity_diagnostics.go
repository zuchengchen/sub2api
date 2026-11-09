package service

import (
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type gpt61SolChatIdentityValueSummary struct {
	Present  bool   `json:"present"`
	Examined bool   `json:"examined"`
	Valid    bool   `json:"valid"`
	Bytes    int    `json:"bytes"`
	HMAC     string `json:"hmac_sha256,omitempty"`
}

type gpt61SolChatIdentitySummary struct {
	Valid         bool                                        `json:"valid"`
	SessionSource string                                      `json:"session_source"`
	ThreadSource  string                                      `json:"thread_source"`
	TurnSource    string                                      `json:"turn_source"`
	Session       gpt61SolChatIdentityValueSummary            `json:"session"`
	Thread        gpt61SolChatIdentityValueSummary            `json:"thread"`
	Turn          gpt61SolChatIdentityValueSummary            `json:"turn"`
	Sources       map[string]gpt61SolChatIdentityValueSummary `json:"sources"`
}

func summarizeGPT61SolChatIdentityValue(raw string, present bool, valid bool) gpt61SolChatIdentityValueSummary {
	out := gpt61SolChatIdentityValueSummary{Present: present, Examined: true, Bytes: len(raw)}
	if value := sanitizeGPT61SolChatIdentity(raw); present && valid && value != "" {
		out.Valid = true
		out.HMAC = diagnosticHeaderValueDigest(value)
	}
	return out
}

// Called only after bounded diagnostic admission. All keys/source names are a
// fixed enum; neither client-provided keys nor identity values reach the log.
// This is candidate evidence, not a claim that the feature policy was enabled.
func summarizeGPT61SolChatIdentity(c *gin.Context, body []byte) gpt61SolChatIdentitySummary {
	out := gpt61SolChatIdentitySummary{
		SessionSource: "none", ThreadSource: "none", TurnSource: "none",
		Sources: make(map[string]gpt61SolChatIdentityValueSummary),
	}
	if c == nil || c.Request == nil {
		return out
	}
	for _, name := range gpt61SolChatIdentityHeaders() {
		values := c.Request.Header.Values(name)
		out.Sources["header:"+strings.ToLower(name)] = summarizeGPT61SolChatIdentityValue(c.GetHeader(name), len(values) > 0, len(values) == 1)
	}
	metadata := func(source, raw string, present, single bool) gjson.Result {
		_, valid := gpt61SolChatIdentityMetadata(raw)
		valid = present && single && valid
		// Presence is independent of validity. In particular, an invalid ID
		// must not look absent merely because strict parsing rejected it.
		value := gjson.Result{}
		examined := !present
		if len(raw) <= gpt61SolChatIdentityMaxMetadataBytes && utf8.ValidString(raw) && gjson.Valid(raw) {
			value = gjson.Parse(raw)
			examined = value.IsObject()
		}
		out.Sources[source] = gpt61SolChatIdentityValueSummary{Present: present, Examined: examined, Valid: valid, Bytes: len(raw)}
		for _, field := range []string{"session_id", "thread_id", "turn_id"} {
			v := value.Get(field)
			summary := summarizeGPT61SolChatIdentityValue(v.Str, v.Exists(), valid && v.Type == gjson.String)
			summary.Examined = examined
			if v.Type != gjson.String {
				summary.Bytes = len(v.Raw)
			}
			out.Sources[source+":"+field] = summary
		}
		return value
	}
	values := c.Request.Header.Values(openAIWSTurnMetadataHeader)
	metadata("header:turn_metadata", c.GetHeader(openAIWSTurnMetadataHeader), len(values) > 0, len(values) == 1)
	if len(body) <= gpt61SolChatIdentityMaxBodyBytes && utf8.Valid(body) && gjson.ValidBytes(body) {
		root := gjson.ParseBytes(body)
		raw := root.Get("client_metadata")
		value := metadata("body:client_metadata", raw.Raw, raw.Exists(), root.IsObject() && !hasDuplicateJSONObjectKeys(root))
		embedded := value.Get(gjsonEscapeKey(openAIWSTurnMetadataHeader))
		metadata("body:turn_metadata", embedded.Str, embedded.Exists(), embedded.Type == gjson.String)
		if session, valid := gpt61SolChatClaudeSession(root); session != "" && valid {
			out.Sources["body:claude_metadata_session"] = summarizeGPT61SolChatIdentityValue(session, true, true)
		}
	}
	resolved := resolveGPT61SolChatIdentity(c, body)
	out.Valid = resolved.valid
	if !resolved.valid {
		return out
	}
	out.SessionSource, out.ThreadSource, out.TurnSource = resolved.sessionSource, resolved.threadSource, resolved.turnSource
	ev := resolved.evidence
	out.Session = summarizeGPT61SolChatIdentityValue(ev.ClientSessionID, ev.ClientSessionID != "", true)
	out.Thread = summarizeGPT61SolChatIdentityValue(ev.ClientThreadID, ev.ClientThreadID != "", true)
	out.Turn = summarizeGPT61SolChatIdentityValue(ev.ClientTurnID, ev.ClientTurnID != "", true)
	return out
}
