package service

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	openAIWSReadDiagnosticKey     = "openai_ws_read_diagnostic"
	openAIWSReadDiagnosticKind    = "stream_read_diagnostic"
	openAIWSReadDiagnosticStage   = "ws_response"
	openAIWSReadDiagnosticReason  = "incomplete_after_progress"
	openAIWSReadDiagnosticMessage = "Upstream WebSocket response ended after progress"
)

func openAIWSReadDiagnosticContextEligible(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.Context().Err() != nil ||
		GetOpenAIClientTransport(c) != OpenAIClientTransportHTTP ||
		IsOpsRequestScopedError(c) || GetOpsCyberPolicy(c) != nil ||
		OpsClientBusinessLimitedReason(c) == OpsClientBusinessLimitedReasonLocalModelConfiguration {
		return false
	}
	if value, exists := c.Get(OpsStreamTurnKey); exists && value != nil {
		turn, valid := value.(int)
		if !valid || turn != 0 {
			return false
		}
	}
	return true
}

// This producer belongs only to the existing nonretryable HTTP->WS reader
// exit. It keeps an immutable value outside the outcome list and never runs
// error rules, changes scalar attribution, or creates an Ops row.
func recordOpenAIWSReadDiagnostic(c *gin.Context, account *Account, boundary OpsReplayBoundary) {
	if !openAIWSReadDiagnosticContextEligible(c) || account == nil || !account.IsOpenAIOAuthLike() {
		return
	}
	if _, exists := c.Get(openAIWSReadDiagnosticKey); exists {
		return
	}
	proxyID, proxyName := opsUpstreamWSProxyAttribution(account)
	event := cloneOpenAIWSReadDiagnostic(&OpsUpstreamErrorEvent{
		AtUnixMs: time.Now().UnixMilli(), Platform: PlatformOpenAI,
		AccountID: account.ID, AccountName: account.Name,
		ProxyID: proxyID, ProxyName: proxyName,
		Kind: openAIWSReadDiagnosticKind, Stage: openAIWSReadDiagnosticStage,
		Reason: openAIWSReadDiagnosticReason, Message: openAIWSReadDiagnosticMessage,
		ReplayBoundary: &boundary,
	})
	if event != nil {
		c.Set(openAIWSReadDiagnosticKey, event)
	}
}

// AttachOpenAIWSReadDiagnostic runs only after an existing HTTP Ops row has
// passed monitoring, outcome classification and usage-policy processing.
// The sidecar becomes JSON during queue sanitization; outcome consumers never
// see it. A consumed tombstone prevents a second row from borrowing the value.
func AttachOpenAIWSReadDiagnostic(c *gin.Context, entry *OpsInsertErrorLogInput) {
	if entry == nil || !openAIWSReadDiagnosticContextEligible(c) {
		return
	}
	raw, exists := c.Get(openAIWSReadDiagnosticKey)
	if !exists {
		return
	}
	event, _ := raw.(*OpsUpstreamErrorEvent)
	if event == nil {
		return
	}
	c.Set(openAIWSReadDiagnosticKey, nil)
	// Already serialized entries keep their exact JSON, including earlier
	// dropped-attempt counts. Optional diagnostics cannot replace that evidence.
	if entry.UpstreamErrorsJSON != nil || entry.upstreamReadDiagnostic != nil {
		return
	}
	entry.upstreamReadDiagnostic = cloneOpenAIWSReadDiagnostic(event)
}

func cloneOpenAIWSReadDiagnostic(event *OpsUpstreamErrorEvent) *OpsUpstreamErrorEvent {
	if event == nil || event.Kind != openAIWSReadDiagnosticKind ||
		event.Stage != openAIWSReadDiagnosticStage || event.Reason != openAIWSReadDiagnosticReason {
		return nil
	}
	boundary := cloneOpsReplayBoundary(event.ReplayBoundary)
	if boundary == nil {
		return nil
	}
	// Copy only fixed diagnostic fields and the attempt's bounded credential-
	// free attribution. No request ID from a reused handshake, body or raw cause
	// is available through this path. Zero status is not a synthetic HTTP reply.
	out := &OpsUpstreamErrorEvent{
		AtUnixMs: event.AtUnixMs, Platform: PlatformOpenAI,
		AccountID: event.AccountID, AccountName: truncateString(strings.TrimSpace(event.AccountName), 128),
		ProxyName: truncateString(strings.TrimSpace(event.ProxyName), 128),
		Kind:      openAIWSReadDiagnosticKind, Stage: openAIWSReadDiagnosticStage,
		Reason: openAIWSReadDiagnosticReason, Message: openAIWSReadDiagnosticMessage,
		ReplayBoundary: boundary,
	}
	if event.ProxyID != nil {
		proxyID := *event.ProxyID
		out.ProxyID = &proxyID
	}
	if out.AccountID < 0 {
		out.AccountID = 0
	}
	if out.AtUnixMs < 0 {
		out.AtUnixMs = 0
	}
	normalizeOpsUpstreamProxyAttribution(out)
	return out
}
