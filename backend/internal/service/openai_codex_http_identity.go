package service

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Stage real bridge session evidence before identity isolation and version
// selection. A cache key or equal message text does not identify a conversation.
// Anonymous requests get an attempt-stable seed, never an account-wide session.
func stageCodexHTTPClientSession(c *gin.Context, account *Account, body []byte) func() {
	restore := func() {}
	if c == nil || c.Request == nil || account == nil || !account.UsesOpenAICodexProtocol() {
		return restore
	}
	parsed := gjson.ParseBytes(body)
	ev := collectCodexTurnEvidence(parsed.Get("client_metadata"), c.Request.Header, parsed)
	session := firstNonEmptyString(ev.ClientSessionID, ev.ClientThreadID)
	thread := firstNonEmptyString(ev.ClientThreadID, session)
	if session != "" {
		c.Set("codex_http_session_scope", session+"\x00"+thread)
	} else if ev.ClientTurnID != "" {
		c.Set("codex_http_session_scope", "turn:"+ev.ClientTurnID)
	}
	if ev.ClientSessionID != "" || ev.ClientThreadID != "" || ev.ClientTurnID != "" {
		return restore
	}
	session = strings.TrimSpace(extractClaudeCodeSessionID(c, body))
	if session == "" {
		session = codexAnonymousRequestSession(c)
	}
	c.Set("codex_http_session_scope", session+"\x00"+session)
	original := c.Request.Header
	headers := original.Clone()
	headers.Set("session-id", session)
	c.Request.Header = headers
	return func() { c.Request.Header = original }
}
