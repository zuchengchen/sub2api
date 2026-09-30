package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var excelBPSReplay basispoints.ReplayCache

// errExcelBPSHTTPFallback marks a BPS failure that certainly did not execute
// the request, so Forward may continue on the same account's next route.
var errExcelBPSHTTPFallback = errors.New("excel BPS fallback to HTTP")

const excelBPSHTTPFallbackReason = "bps_error"

// excelBPSUncertainFailoverReason marks a BPS attempt that may already have
// executed upstream. It must never be replayed on the same account.
const excelBPSUncertainFailoverReason = GatewayFailureReason("basispoints_execution_uncertain")

// excelBPSSendTrace records, through net/http/httptrace, how far the BPS
// request got. Transport callbacks can run on other goroutines (HTTP/2).
type excelBPSSendTrace struct {
	headers atomic.Bool // request headers were handed to the connection
	sent    atomic.Bool // WroteRequest reported the whole request written
}

func (t *excelBPSSendTrace) attach(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteHeaders: func() { t.headers.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				t.sent.Store(true)
			}
		},
	})
}

// mayHaveExecuted classifies a failed round trip. A request that was never
// completely written cannot have been executed; one that was written may have
// been, even though no response arrived. HTTP/2 returns a context cancellation
// before its writer goroutine reports WroteRequest, so written headers plus a
// cancellation (with the caller's context still alive) are also uncertain.
func (t *excelBPSSendTrace) mayHaveExecuted(err error) bool {
	if t.sent.Load() {
		return true
	}
	return t.headers.Load() && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

// excelBPSCanFallbackToHTTP reports whether no model output reached the client,
// so the request may still move to another route or account. SSE keepalive
// comments are not output: like the Codex HTTP stream heartbeat they are
// counted in openAIStreamKeepaliveBytesKey and excluded here.
func excelBPSCanFallbackToHTTP(c *gin.Context) bool {
	if c == nil || c.Writer == nil {
		return true
	}
	if IsResponseCommitted(c) {
		return false
	}
	return OpenAICompactKeepaliveAdjustedWrittenSize(c) < 0
}

// excelBPSDropUnsentStreamHeaders removes the SSE headers this attempt set
// before anything was written, so a later JSON error (or another account's
// non-stream response) is not labeled as an event stream.
func excelBPSDropUnsentStreamHeaders(c *gin.Context) {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return
	}
	h := c.Writer.Header()
	if h.Get("Content-Type") == "text/event-stream" {
		h.Del("Content-Type")
	}
	if h.Get("Cache-Control") == "no-cache" {
		h.Del("Cache-Control")
	}
	if h.Get("X-Accel-Buffering") == "no" {
		h.Del("X-Accel-Buffering")
	}
}

func (s *OpenAIGatewayService) disableExcelBPSOn403(ctx context.Context, account *Account) bool {
	if !account.IsExcelBPSAutoDisableOn403Enabled() {
		return false
	}
	repo, ok := s.accountRepo.(AccountExcelBPSRepository)
	if !ok {
		return false
	}
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	changed, err := repo.DisableExcelBPSOn403(stateCtx, account)
	if err != nil {
		// Do not log upstream bodies, credentials or database query arguments.
		logger.LegacyPrintf("service.openai_excel_bps", "auto-disable failed: account_id=%d error_type=%T", account.ID, err)
		return false
	}
	if changed {
		logger.LegacyPrintf("service.openai_excel_bps", "automatically disabled Excel BPS after upstream HTTP 403: account_id=%d", account.ID)
	}
	return changed
}

func (s *OpenAIGatewayService) excelBPSImageRelay(ctx context.Context) (*basispoints.ImageRelay, error) {
	settings, err := s.settingService.GetExcelBPSImageRelaySettings(ctx)
	if err != nil || !settings.Enabled {
		return nil, err
	}
	s.excelBPSImagesMu.Lock()
	defer s.excelBPSImagesMu.Unlock()
	if s.excelBPSImages == nil {
		dataDir := strings.TrimSpace(os.Getenv("DATA_DIR"))
		if dataDir == "" {
			dataDir = "./data"
		}
		s.excelBPSImages, err = basispoints.NewImageRelay(settings.BaseURL, filepath.Join(dataDir, "bps-images"))
	} else {
		err = s.excelBPSImages.SetPublicOrigin(settings.BaseURL)
	}
	return s.excelBPSImages, err
}

func (s *OpenAIGatewayService) CloseExcelBPSImages() error {
	if s == nil {
		return nil
	}
	s.excelBPSImagesMu.Lock()
	defer s.excelBPSImagesMu.Unlock()
	return s.excelBPSImages.Close()
}

// ServeExcelBPSImage allows the upstream to retrieve an unguessable temporary URL.
func (s *OpenAIGatewayService) ServeExcelBPSImage(c *gin.Context) {
	relay, _ := s.excelBPSImageRelay(c.Request.Context())
	relay.ServeHTTP(c.Writer, c.Request)
}

func excelBPSAccountID(account *Account, accessToken string) string {
	if accountID := strings.TrimSpace(account.GetChatGPTAccountID()); accountID != "" {
		return accountID
	}
	claims, err := openai.DecodeIDToken(accessToken)
	if err != nil || claims.OpenAIAuth == nil {
		return ""
	}
	return strings.TrimSpace(claims.OpenAIAuth.ChatGPTAccountID)
}

func newExcelBPSRequest(ctx context.Context, body []byte, token, accountID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, basispoints.ResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = http.Header{
		"Authorization": {"Bearer " + token}, "Chatgpt-Account-Id": {accountID}, "X-Openai-Account-Id": {accountID},
		"X-Basispoints-Auth-Mode": {"chatgpt"}, "Content-Type": {"application/json"}, "Accept": {"text/event-stream"},
		"Origin": {"https://bps.openai.com"}, "User-Agent": {"Mozilla/5.0"},
		"X-Openai-Internal-Basispoints-Client-Product":       {"basispoints-excel-plugin"},
		"X-Openai-Internal-Basispoints-Client-Agent-Profile": {"excel"},
	}
	return req, nil
}

// BPS deliberately bypasses Codex ticket/cookie injection and OAuth plugins:
// only the selected account's bearer and ChatGPT account ID belong on this host.
//
// Failure contract (see Forward): errExcelBPSHTTPFallback only when BPS
// certainly did not execute the request; *UpstreamFailoverError when it may
// have and nothing reached the client; otherwise the error is committed here.
func (s *OpenAIGatewayService) forwardExcelBPS(ctx context.Context, c *gin.Context, account *Account, body []byte, start time.Time) (*OpenAIForwardResult, error) {
	// commit reports a failure on this response once the client can no longer
	// be moved to another route.
	commit := func(keepaliveCommitted bool, status int, code, message string) (*OpenAIForwardResult, error) {
		MarkResponseCommitted(c)
		if keepaliveCommitted {
			writeOpenAICompactSSEFailureMessage(c, status, code, message)
		} else {
			c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
		}
		return nil, fmt.Errorf("excel BPS: %s", code)
	}
	// fail: BPS certainly did not execute the request (not sent, or an explicit
	// upstream rejection). The same account may try its next route.
	fail := func(status int, code, message string) (*OpenAIForwardResult, error) {
		keepaliveCommitted := StopOpenAICompactSSEKeepaliveCommitted(c)
		if excelBPSCanFallbackToHTTP(c) && !keepaliveCommitted {
			logger.LegacyPrintf("service.openai_excel_bps", "falling back to Codex HTTP: account_id=%d code=%s", account.ID, code)
			return nil, fmt.Errorf("%w: %s", errExcelBPSHTTPFallback, code)
		}
		return commit(keepaliveCommitted, status, code, message)
	}
	// failUncertain: BPS may have executed the request. Replaying it on this
	// account could run the prompt twice, so only another account may retry.
	failUncertain := func(upstreamStatus int, requestID, code, message, cause string) (*OpenAIForwardResult, error) {
		keepaliveCommitted := StopOpenAICompactSSEKeepaliveCommitted(c)
		if excelBPSCanFallbackToHTTP(c) && !keepaliveCommitted {
			// Only keepalive comments (if anything) reached the client: another
			// account can still answer inside the same SSE response.
			logger.LegacyPrintf("service.openai_excel_bps", "switching account after uncertain Excel BPS failure: account_id=%d code=%s", account.ID, code)
			excelBPSDropUnsentStreamHeaders(c)
			return nil, newExcelBPSUncertainFailover(c, account, upstreamStatus, requestID, code, message, cause)
		}
		return commit(keepaliveCommitted, http.StatusBadGateway, code, message)
	}
	originalModel := gjson.GetBytes(body, "model").String()
	model := account.GetMappedModel(originalModel)
	stream := gjson.GetBytes(body, "stream").Bool()
	var err error
	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return fail(400, "basispoints_request_invalid", "Invalid model request")
	}
	identity, _ := resolveOpenAIWSExecutionScope(c, body, getAPIKeyIDFromContext(c))
	if identity != "" {
		body, err = sjson.SetBytes(body, "prompt_cache_key", identity)
		if err != nil {
			return nil, err
		}
	}
	if isOpenAIResponsesCompactPath(c) {
		var request map[string]any
		if err = json.Unmarshal(body, &request); err != nil {
			return fail(400, "basispoints_request_invalid", "Invalid compact request")
		}
		var input []any
		switch v := request["input"].(type) {
		case []any:
			input = v
		case string:
			input = []any{map[string]any{"role": "user", "content": v}}
		default:
			return fail(400, "basispoints_request_invalid", "Compact requires input")
		}
		request["input"] = append(input, map[string]any{"type": "compaction_trigger"})
		request["tool_choice"] = "none"
		body, err = json.Marshal(request)
		if err != nil {
			return nil, err
		}
	}
	scope := fmt.Sprintf("account:%d/key:%d/thread:%s", account.ID, getAPIKeyIDFromContext(c), identity)
	relay, err := s.excelBPSImageRelay(ctx)
	if err != nil {
		return fail(503, "basispoints_image_relay_unavailable", err.Error())
	}
	body, err = relay.Rewrite(body, scope)
	if err != nil {
		if errors.Is(err, basispoints.ErrImageRelayFull) {
			return fail(503, "basispoints_image_relay_full", err.Error())
		}
		if errors.Is(err, basispoints.ErrImageRelayStorage) {
			return fail(503, "basispoints_image_relay_unavailable", err.Error())
		}
		return fail(400, "basispoints_request_invalid", err.Error())
	}
	upstreamBody, bridge, err := basispoints.Prepare(body, scope, &excelBPSReplay)
	if err != nil {
		return fail(400, "basispoints_request_invalid", err.Error())
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return fail(502, "basispoints_auth_unavailable", "Account OAuth credential is unavailable")
	}
	accountID := excelBPSAccountID(account, token)
	if accountID == "" {
		return fail(400, "basispoints_account_id_missing", "Excel BPS requires chatgpt_account_id")
	}
	var sendTrace excelBPSSendTrace
	requestCtx := sendTrace.attach(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileLongStream)))
	req, err := newExcelBPSRequest(requestCtx, upstreamBody, token, accountID)
	if err != nil {
		return nil, err
	}
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	SetActualOpenAIUpstreamEndpoint(c, "/basispoints/api/responses")
	SetOpsUpstreamModel(c, model)
	sent := time.Now()
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(sent).Milliseconds())
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			// The client is gone: neither another route nor another account.
			return nil, ctx.Err()
		}
		if sendTrace.mayHaveExecuted(err) {
			return failUncertain(0, "", "basispoints_transport_error", "Excel BPS connection was interrupted",
				"transport error after the request was written: "+sanitizeUpstreamErrorMessage(err.Error()))
		}
		return fail(http.StatusBadGateway, "basispoints_transport_error", "Excel BPS connection failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
		// Preserve the original rejection for Ops without exposing it to clients.
		// BPS errors can echo request fields, so redact before storing diagnostics.
		upstreamMessage := fmt.Sprintf("Excel BPS returned HTTP %d", resp.StatusCode)
		upstreamDetail := ""
		if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
			safeBody := excelBPSSanitizeErrorBody(string(raw), token, account)
			maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
			if maxBytes <= 0 {
				maxBytes = 2048
			}
			upstreamDetail, _ = sanitizeErrorBodyForStorage(safeBody, maxBytes)
			if message := strings.TrimSpace(extractUpstreamErrorMessage([]byte(safeBody))); message != "" {
				upstreamMessage = truncateString(message, 2048)
			}
		}
		setOpsUpstreamError(c, resp.StatusCode, upstreamMessage, upstreamDetail)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
			ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
			UpstreamStatusCode: resp.StatusCode, UpstreamRequestID: resp.Header.Get("x-request-id"),
			UpstreamURL: basispoints.ResponsesURL, Kind: "http_error",
			Message: upstreamMessage, Detail: upstreamDetail, UpstreamResponseBody: upstreamDetail,
		})
		code := gjson.GetBytes(raw, "error.code").String()
		if code == "basispoints_model_access_changed" {
			return fail(resp.StatusCode, code, "This model is not available on the account's Excel BPS endpoint")
		}
		message := "Excel BPS rejected this request; account scheduling was not changed"
		if resp.StatusCode == http.StatusForbidden && s.disableExcelBPSOn403(ctx, account) {
			message = "Excel BPS rejected this request; Excel BPS was automatically disabled for this account"
		}
		return fail(resp.StatusCode, "basispoints_upstream_error", message)
	}
	upstreamRequestID := resp.Header.Get("x-request-id")
	converted := bridge.Stream(resp.Body)
	defer func() { _ = converted.Close() }()
	// The bridge sees the body after group policy mapping. Keep the original
	// client effort for usage display, and the BPS-normalized effort for billing.
	requestedEffort := coalesceRequestedReasoningEffort(RequestedReasoningEffortFromContext(ctx), &bridge.RequestedEffort)
	result := &OpenAIForwardResult{Model: originalModel, UpstreamModel: model, UpstreamEndpoint: "/basispoints/api/responses", Stream: stream, ReasoningEffort: &bridge.Effort, RequestedReasoningEffort: requestedEffort, RequestID: upstreamRequestID}
	if stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
	}
	scanner := newOpenAISSEReadPump(converted, 16<<20)
	defer scanner.Close()
	heartbeatInterval := 15 * time.Second
	if s.excelBPSHeartbeatInterval > 0 {
		heartbeatInterval = s.excelBPSHeartbeatInterval
	}
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	keepalive := func() {
		if stream && ctx.Err() == nil {
			n, _ := c.Writer.WriteString(": keepalive\n\n")
			// Heartbeat bytes are not model output (see excelBPSCanFallbackToHTTP).
			recordOpenAIStreamKeepaliveBytes(c, n)
			c.Writer.Flush()
		}
	}
	var completed, terminalPayload []byte
	terminal := ""
	cacheCreationAsInput := account.IsExcelBPSCacheCreationAsInputEnabled()
	for scanner.Next(ctx, 0, heartbeat.C, keepalive) {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			payload := []byte(strings.TrimPrefix(line, "data: "))
			kind := gjson.GetBytes(payload, "type").String()
			s.parseSSEUsageBytes(payload, &result.Usage)
			if cacheCreationAsInput {
				payload, err = excelBPSDownstreamUsage(payload)
				if err != nil {
					return failUncertain(http.StatusBadGateway, upstreamRequestID, "basispoints_usage_invalid",
						"Excel BPS usage could not be normalized", "usage normalization failed after a 2xx response")
				}
				line = "data: " + string(payload)
			}
			if result.FirstTokenMs == nil && (kind == "response.output_text.delta" || kind == "response.output_item.added") {
				ms := int(time.Since(start).Milliseconds())
				result.FirstTokenMs = &ms
			}
			switch kind {
			case "response.completed", "response.failed", "response.incomplete", "error":
				terminal = kind
				terminalPayload = payload
				completed = []byte(gjson.GetBytes(payload, "response").Raw)
				result.ResponseID = gjson.GetBytes(payload, "response.id").String()
				result.UpstreamResponseModel = gjson.GetBytes(payload, "response.model").String()
			}
		}
		if stream {
			if _, err = c.Writer.WriteString(line + "\n"); err != nil {
				result.ClientDisconnect = true
				result.Duration = time.Since(start)
				return result, err
			}
			if line == "" {
				c.Writer.Flush()
			}
		}
	}
	result.Duration = time.Since(start)
	result.UpstreamTerminalEvent = terminal
	if err = scanner.Err(); err != nil || terminal == "" {
		if ctx.Err() != nil {
			result.ClientDisconnect = true
			return result, ctx.Err()
		}
		cause := "stream ended before a terminal event"
		if err != nil {
			cause = "stream read failed before a terminal event: " + sanitizeUpstreamErrorMessage(err.Error())
		}
		return failUncertain(http.StatusBadGateway, upstreamRequestID, "basispoints_stream_incomplete", "Excel BPS stream ended before completion", cause)
	}
	if terminal != "response.completed" {
		if stream && !excelBPSCanFallbackToHTTP(c) {
			// The terminal event already reached the client.
			MarkResponseCommitted(c)
			return result, fmt.Errorf("excel BPS terminal: %s", terminal)
		}
		errorBody, rejected := excelBPSTerminalRejection(terminal, terminalPayload)
		if rejected {
			// Every account would reject this request: report it, never retry.
			status, code, message := excelBPSTerminalRejectionClientError(errorBody, token, account)
			return commit(StopOpenAICompactSSEKeepaliveCommitted(c), status, code, message)
		}
		cause := "terminal=" + terminal
		if code := excelBPSSafeErrorCode(errorBody); code != "" {
			cause += " code=" + code
		}
		if reason := gjson.GetBytes(terminalPayload, "response.incomplete_details.reason").String(); excelBPSSafeCodePattern.MatchString(reason) {
			cause += " reason=" + reason
		}
		return failUncertain(http.StatusBadGateway, upstreamRequestID, "basispoints_protocol_error", "Excel BPS did not complete the response", cause)
	}
	if !stream {
		c.Data(200, "application/json", completed)
	}
	s.bindHTTPResponseAccount(ctx, c, account, result.ResponseID)
	return result, nil
}

// newExcelBPSUncertainFailover moves a possibly executed BPS attempt to another
// account. It carries no same-account retry, cooldown or credential signal:
// the handler excludes this account for the rest of the request and reports an
// ordinary scheduling failure, exactly like a Codex HTTP stream truncation.
func newExcelBPSUncertainFailover(c *gin.Context, account *Account, upstreamStatus int, requestID, code, message, cause string) *UpstreamFailoverError {
	opsMessage := "Excel BPS attempt may have executed; switching account: " + message
	setOpsUpstreamError(c, upstreamStatus, opsMessage, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: account.Platform, AccountID: account.ID, AccountName: account.Name,
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		UpstreamStatusCode: upstreamStatus, UpstreamRequestID: requestID,
		UpstreamURL: basispoints.ResponsesURL, Kind: "failover",
		Reason: string(excelBPSUncertainFailoverReason), Message: opsMessage, Detail: cause,
	})
	body, err := json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "code": code, "message": message}})
	if err != nil {
		body = []byte(`{"error":{"type":"upstream_error","message":"Excel BPS connection was interrupted"}}`)
	}
	headers := http.Header{}
	if requestID = strings.TrimSpace(requestID); requestID != "" {
		headers.Set("x-request-id", requestID)
	}
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           body,
		ResponseHeaders:        headers,
		RetryableOnSameAccount: false,
		RequestScopedTransient: false,
		Scope:                  GatewayFailureScopeAccount,
		Reason:                 excelBPSUncertainFailoverReason,
		NextAccountAction:      NextAccountRetry,
		ClientStatusCode:       http.StatusBadGateway,
		ClientMessage:          message,
	}
}

var excelBPSSafeCodePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func excelBPSSafeErrorCode(errorBody []byte) string {
	code := strings.TrimSpace(gjson.GetBytes(errorBody, "error.code").String())
	if !excelBPSSafeCodePattern.MatchString(code) {
		return ""
	}
	return code
}

// excelBPSTerminalError normalizes the error of a non-completed terminal event
// to {"error":{...}}. It is nil when the event carries no error, for example a
// response.incomplete.
func excelBPSTerminalError(terminal string, payload []byte) []byte {
	for _, path := range []string{"response.error", "error"} {
		if object := gjson.GetBytes(payload, path); object.IsObject() {
			return []byte(`{"error":` + object.Raw + `}`)
		}
	}
	if terminal != "error" {
		return nil
	}
	// Responses streams also use the flat {"type":"error","code":...,"message":...}.
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil {
		return nil
	}
	delete(fields, "type")
	delete(fields, "sequence_number")
	flat, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return append(append([]byte(`{"error":`), flat...), '}')
}

// excelBPSTerminalRejection reports whether a non-completed terminal event
// rejects the request itself, so every account would fail it the same way. It
// reuses the Codex HTTP classification of a pre-output response.failed
// (openAIStreamFailedEventShouldFailover): context window, encrypted content,
// cyber/usage policy and invalid_request are request-scoped. Server errors,
// rate limits, bridge protocol errors and error-less incomplete responses are
// not, and stay uncertain.
func excelBPSTerminalRejection(terminal string, payload []byte) ([]byte, bool) {
	errorBody := excelBPSTerminalError(terminal, payload)
	if errorBody == nil {
		return nil, false
	}
	return errorBody, !openAIStreamFailedEventShouldFailover(errorBody, extractOpenAISSEErrorMessage(errorBody))
}

// excelBPSTerminalRejectionClientError is the non-stream client error for a
// request-scoped terminal rejection. Streaming clients receive the terminal
// event itself; this sanitized copy exposes no more than that.
func excelBPSTerminalRejectionClientError(errorBody []byte, token string, account *Account) (int, string, string) {
	status := http.StatusBadRequest
	if openAIStreamFailedEventSemanticStatus(errorBody, extractOpenAISSEErrorMessage(errorBody)) == http.StatusForbidden {
		status = http.StatusForbidden
	}
	code, message := "basispoints_request_invalid", "Excel BPS rejected this request"
	safe := []byte(excelBPSSanitizeErrorBody(string(errorBody), token, account))
	if value := excelBPSSafeErrorCode(safe); value != "" {
		code = value
	}
	if value := strings.TrimSpace(gjson.GetBytes(safe, "error.message").String()); value != "" {
		message = value
	}
	return status, code, message
}

var excelBPSBearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[^\s"',;<>]+`)
var excelBPSURLCredentialsPattern = regexp.MustCompile(`(https?://)[^/\s@]+@`)
var excelBPSImageCapabilityPattern = regexp.MustCompile(`/api/bps-images/[A-Za-z0-9_-]+`)

func excelBPSSanitizeErrorBody(raw, token string, account *Account) string {
	if !json.Valid([]byte(raw)) {
		return ""
	}
	secrets := append([]string{token}, excelBPSAccountSecrets(account)...)
	fields := make(map[string]string)
	for _, key := range []string{"message", "code", "type", "param"} {
		value := gjson.Get(raw, "error."+key)
		if value.Type != gjson.String {
			continue
		}
		clean := value.String()
		for _, secret := range secrets {
			if secret != "" {
				clean = strings.ReplaceAll(clean, secret, "[redacted]")
			}
		}
		clean = excelBPSBearerPattern.ReplaceAllString(clean, "Bearer [redacted]")
		clean = excelBPSURLCredentialsPattern.ReplaceAllString(clean, "${1}[redacted]@")
		clean = excelBPSImageCapabilityPattern.ReplaceAllString(clean, "/api/bps-images/[redacted]")
		clean = sanitizeUpstreamErrorMessage(clean)
		fields[key] = truncateString(logredact.RedactText(clean, "authorization", "api_key", "apikey", "token", "secret", "key", "cookie", "ticket", "recovery_ticket"), 2048)
	}
	encoded, _ := json.Marshal(map[string]any{"error": fields})
	return string(encoded)
}

func excelBPSAccountSecrets(account *Account) []string {
	var secrets []string
	for _, key := range []string{"access_token", "refresh_token", "id_token", "api_key", "session_key", "cookie"} {
		if value := account.GetCredential(key); value != "" {
			secrets = append(secrets, value)
		}
	}
	if account.Proxy != nil && account.Proxy.Password != "" {
		secrets = append(secrets, account.Proxy.Password)
	}
	return secrets
}
