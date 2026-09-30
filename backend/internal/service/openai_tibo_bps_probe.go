package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
)

// probeOpenAITiboBPS sends the fixed Tibo question (gpt-6-astra, effort low)
// once through the account's Excel BPS route. It never changes account state.
//
// It measures the business BPS path (same token, ChatGPT account ID, request
// headers, HTTP profile and account proxy as forwardExcelBPS) but has no gin
// context, records no ops errors or usage, never auto-disables BPS on 403,
// never binds the response to the account and uses a private replay cache.
func (s *OpenAIGatewayService) probeOpenAITiboBPS(ctx context.Context, account *Account) openAITiboProbeSample {
	unknown := openAITiboProbeSample{verdict: openAITiboUnknown}
	if s == nil || s.httpUpstream == nil || account == nil {
		return unknown
	}
	body, err := json.Marshal(openAICookieWSProbePayload(false))
	if err != nil {
		return unknown
	}
	var replay basispoints.ReplayCache
	upstreamBody, bridge, err := basispoints.Prepare(body, fmt.Sprintf("tibo-probe:account:%d", account.ID), &replay)
	if err != nil {
		return unknown
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil || token == "" {
		return unknown
	}
	accountID := excelBPSAccountID(account, token)
	if accountID == "" {
		return unknown
	}
	requestCtx := WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileLongStream))
	req, err := newExcelBPSRequest(requestCtx, upstreamBody, token, accountID)
	if err != nil {
		return unknown
	}
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil || resp == nil || resp.Body == nil {
		if resp != nil {
			unknown.status = resp.StatusCode
		}
		return unknown
	}
	unknown.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		unknown.retryAt = parseRetryAfterResetTime(resp.Header, time.Now())
		return unknown
	}
	// The bridge re-emits Responses SSE: response.completed keeps response.model
	// and the assistant message output_text, which the observer classifies.
	converted := bridge.Stream(resp.Body)
	defer func() { _ = converted.Close() }()
	data, err := io.ReadAll(io.LimitReader(converted, openAICodexTicketProbeBodyLimit+1))
	if err != nil {
		return unknown
	}
	// Same classification as the HTTP probe: healthy only for True from astra.
	return openAITiboSampleFromObservation(resp.StatusCode, observeOpenAICookieWSHTTPProbe(data))
}
