package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAIAccessAutoVerifiedUntil reports only a live automatic proof for the
// account's current credentials, without making an upstream or database call.
func (s *AccountTestService) OpenAIAccessAutoVerifiedUntil(account *Account) *time.Time {
	if s == nil || s.openaiGatewayService == nil || s.openaiGatewayService.openAITokenProvider == nil {
		return nil
	}
	api := s.openaiGatewayService.openAITokenProvider.refreshAPI
	r := api.openAIRejectionForAccount(account)
	if r == nil || !r.autoVerified || !r.accessVerifiedAt(account, time.Now()) {
		return nil
	}
	until := r.verifiedAccessUntil
	if expiry := openAIAccessTokenExpiresAt(account); expiry != nil && expiry.Before(until) {
		until = *expiry
	}
	return &until
}

const (
	openAIAccessProbeModel     = "gpt-6.1-sol"
	openAIAccessProbeBodyLimit = 256 << 10
)

// Use the account's own stored access token and normal Codex HTTP transport.
// This diagnostic neither calls the token provider nor recovers or refreshes
// credentials. The worker commits any proof against the triggering rejection.
func (s *AccountTestService) probeRejectedOpenAIAccess(ctx context.Context, account *Account) openAIAccessProbeResult {
	if s == nil || s.httpUpstream == nil || !canRecoverOpenAIOAuth(account) || account.GetOpenAIAccessToken() == "" {
		return openAIAccessProbeResult{outcome: openAIAccessProbeSkipped}
	}
	// A v1 plugin reports the request handed to the plugin, not the identity
	// on its final outbound request. Observe the chosen route exactly once.
	if s.pluginManager.ShouldRouteOpenAIOAuth(account) {
		return openAIAccessProbeResult{outcome: openAIAccessProbeSkipped}
	}
	if account.ProxyID != nil && account.Proxy == nil {
		// Do not send a credential directly if the configured proxy is missing.
		return openAIAccessProbeResult{outcome: openAIAccessProbeRetry}
	}

	payload := createOpenAITestPayload(openAIAccessProbeModel, true)
	payload["instructions"] = "Reply only with OK."
	payload["reasoning"] = map[string]string{"effort": "low"}
	body, err := json.Marshal(payload)
	if err != nil {
		return openAIAccessProbeResult{outcome: openAIAccessProbeSkipped}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexAPIURL, bytes.NewReader(body))
	if err != nil {
		return openAIAccessProbeResult{outcome: openAIAccessProbeSkipped}
	}
	req.Header.Set("Authorization", "Bearer "+account.GetOpenAIAccessToken())
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	setOpenAIChatGPTAccountHeaders(req.Header, account)
	account.ApplyHeaderOverrides(req.Header)
	if !sentOpenAIAccessTokenMatches(account, openAITestResponseAuthorization(account, req)) {
		return openAIAccessProbeResult{outcome: openAIAccessProbeSkipped}
	}
	resp, err := s.codexProbeGateway(nil).doOpenAICodexProbe(req, resolveAccountProxyURL(account), account)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil || resp == nil || ctx.Err() != nil {
		return openAIAccessProbeResult{outcome: openAIAccessProbeRetry}
	}
	authorization := openAITestResponseAuthorization(account, resp.Request)
	if !sentOpenAIAccessTokenMatches(account, authorization) {
		return openAIAccessProbeResult{outcome: openAIAccessProbeSkipped}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return openAIAccessProbeResult{outcome: openAIAccessProbeUnauthorized, authorization: authorization}
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 {
		return openAIAccessProbeResult{outcome: openAIAccessProbeRetry}
	}
	if resp.StatusCode != http.StatusOK {
		return openAIAccessProbeResult{outcome: openAIAccessProbeSkipped}
	}
	if resp.Body == nil {
		return openAIAccessProbeResult{outcome: openAIAccessProbeRetry}
	}
	limited := &io.LimitedReader{R: resp.Body, N: openAIAccessProbeBodyLimit + 1}
	hasText := false
	suffix, err := readOpenAIAccountTestStream(limited, func(delta string) {
		hasText = hasText || strings.TrimSpace(delta) != ""
	})
	if err != nil || limited.N == 0 || ctx.Err() != nil || (!hasText && strings.TrimSpace(suffix) == "") {
		return openAIAccessProbeResult{outcome: openAIAccessProbeRetry}
	}
	return openAIAccessProbeResult{outcome: openAIAccessProbeVerified, authorization: authorization}
}
