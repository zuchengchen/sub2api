package service

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// openAIWSUpstreamForbiddenReason classifies a 403 on the upstream WebSocket
// handshake. It is deliberately distinct from auth_failed (401):
//
//   - 401 has an in-place recovery (token refresh) and is a credential signal.
//   - A handshake 403 from chatgpt.com is usually issued by the edge in front
//     of the Codex backend (Cloudflare challenge / bot-management), scoped to
//     the egress IP and transport fingerprint rather than the account. The same
//     account routinely keeps succeeding on other connections and over HTTP.
//
// Treating it as auth_failed returned the 403 straight to the client with no
// HTTP fallback and no account switch.
const openAIWSUpstreamForbiddenReason = "upstream_forbidden"

const openAIWSDialRejectBodyMaxBytes = 1024

// openAIWSDialForbidden returns the dial error when err is an upstream
// WebSocket handshake rejected with 403.
func openAIWSDialForbidden(err error) (*openAIWSDialError, bool) {
	var dialErr *openAIWSDialError
	if err == nil || !errors.As(err, &dialErr) || dialErr == nil || dialErr.StatusCode != http.StatusForbidden {
		return nil, false
	}
	return dialErr, true
}

// openAIWSDialForbiddenIsEdge reports whether a handshake 403 carries no
// account-level evidence. OpenAI account rejections are structured JSON;
// edge blocks are HTML, bare text, empty, or flagged by cf-mitigated.
func openAIWSDialForbiddenIsEdge(dialErr *openAIWSDialError) bool {
	if dialErr == nil {
		return false
	}
	if strings.TrimSpace(dialErr.ResponseHeaders.Get("cf-mitigated")) != "" {
		return true
	}
	body := []byte(strings.TrimSpace(string(dialErr.ResponseBody)))
	if len(body) == 0 {
		return true
	}
	if isCloudflareBotBlockResponse(body) || isOpenAIEdgeChallenge403(body) {
		return true
	}
	return !gjson.ValidBytes(body)
}

func openAIWSDialRejectClass(dialErr *openAIWSDialError) string {
	if dialErr != nil && dialErr.StatusCode == http.StatusForbidden {
		if openAIWSDialForbiddenIsEdge(dialErr) {
			return "edge_forbidden"
		}
		return "account_forbidden"
	}
	return "handshake_rejected"
}

// recordOpenAIWSDialRejected keeps the upstream handshake evidence (status,
// edge headers, body prefix) on the request's ops attempt trail. Production
// runs at error log level, so the info-level acquire_fail line is not enough
// to tell an edge block from an account rejection after the fact.
func recordOpenAIWSDialRejected(c *gin.Context, account *Account, err error) {
	var dialErr *openAIWSDialError
	if c == nil || account == nil || !errors.As(err, &dialErr) || dialErr == nil || dialErr.StatusCode < http.StatusBadRequest {
		return
	}
	headers := dialErr.ResponseHeaders
	detail := []string{}
	for _, name := range []string{"server", "cf-ray", "cf-mitigated", "content-type"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			detail = append(detail, name+"="+truncateString(value, 128))
		}
	}
	body := strings.TrimSpace(string(dialErr.ResponseBody))
	if body != "" {
		detail = append(detail, "body="+sanitizeUpstreamErrorMessage(truncateString(body, openAIWSDialRejectBodyMaxBytes)))
	}
	proxyID, proxyName := opsUpstreamWSProxyAttribution(account)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            proxyID,
		ProxyName:          proxyName,
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: dialErr.StatusCode,
		UpstreamRequestID:  strings.TrimSpace(headers.Get("x-request-id")),
		Kind:               "ws_dial_rejected",
		Stage:              "ws_handshake",
		Reason:             openAIWSDialRejectClass(dialErr),
		Message:            strings.TrimSpace(err.Error()),
		Detail:             strings.Join(detail, " "),
	})
}

// newOpenAIWSDialForbiddenFailoverError switches a not-yet-started WebSocket
// session to another account. Only a structured (account-level) 403 is fed to
// the account 403 policy; edge blocks never consume the account strike budget.
func (s *OpenAIGatewayService) newOpenAIWSDialForbiddenFailoverError(ctx context.Context, account *Account, canonicalModel string, dialErr *openAIWSDialError) *UpstreamFailoverError {
	shouldDisable := false
	if !openAIWSDialForbiddenIsEdge(dialErr) {
		shouldDisable = s.handleOpenAIAccountUpstreamError(ctx, account, dialErr.StatusCode, dialErr.ResponseHeaders, dialErr.ResponseBody, canonicalModel)
	}
	message := "upstream websocket handshake forbidden"
	if dialErr.Err != nil {
		message = sanitizeUpstreamErrorMessage(strings.TrimSpace(dialErr.Err.Error()))
	}
	return s.newOpenAIAccountFailoverError(account, dialErr.StatusCode, dialErr.ResponseHeaders, dialErr.ResponseBody, message, shouldDisable, false)
}
