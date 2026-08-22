package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	contentmoderationassets "github.com/Wei-Shaw/sub2api/resources/content-moderation"
)

const (
	contentModerationDeepSeekCooldown         = 30 * time.Second
	contentModerationDeepSeekRateCooldown     = 60 * time.Second
	contentModerationDeepSeekHealthTTL        = 15 * time.Minute
	contentModerationDeepSeekFailureTrip      = 3
	contentModerationDeepSeekMaxResponseBytes = 64 * 1024
	contentModerationDeepSeekRetryMinBudget   = 250 * time.Millisecond
)

var contentModerationDeepSeekCategories = map[string]struct{}{
	"safe": {}, "cyber_abuse": {}, "cracking": {}, "security_bypass": {},
	"account_abuse": {}, "sexual_deepfake": {}, "doxxing": {},
	"violent_threat": {}, "self_harm": {}, "weapons": {}, "sexual_content": {},
	"fraud_financial_crime": {}, "controlled_substances": {}, "human_exploitation": {},
	"terrorism_extremism": {}, "illegal_gambling": {}, "forgery_counterfeit": {},
	"corruption_tax_evasion": {}, "hate_harassment": {},
	ContentModerationRestrictedCategory: {},
}

const contentModerationParserStatusNormalizedAllowConfidence = "normalized_allow_confidence"

type ContentModerationReviewAttempt struct {
	Reviewer    string  `json:"reviewer"`
	Provider    string  `json:"provider,omitempty"`
	ChannelID   string  `json:"channel_id"`
	ChannelName string  `json:"channel_name,omitempty"`
	Model       string  `json:"model,omitempty"`
	ChunkIndex  int     `json:"chunk_index,omitempty"`
	ChunkCount  int     `json:"chunk_count,omitempty"`
	Outcome     string  `json:"outcome"`
	Verdict     string  `json:"verdict,omitempty"`
	Role        string  `json:"role,omitempty"`
	Confidence  float64 `json:"confidence,omitempty"`
	LatencyMS   int     `json:"latency_ms"`
	HTTPStatus  int     `json:"http_status,omitempty"`
	Error       string  `json:"error,omitempty"`
}

func stampContentModerationReviewAttemptChunk(attempt *ContentModerationReviewAttempt, input contentModerationSecondLayerInput) {
	if attempt == nil {
		return
	}
	attempt.ChunkIndex = 1
	attempt.ChunkCount = 1
	if input.ChunkCount <= 0 {
		return
	}
	attempt.ChunkIndex = input.ChunkIndex + 1
	attempt.ChunkCount = input.ChunkCount
}

type contentModerationDeepSeekChannelState struct {
	mu sync.Mutex

	configDigest        string
	connectivityDigest  string
	credentialDigest    string
	consecutiveFailures int
	authDisabled        bool
	cooldownUntil       time.Time
	halfOpenProbe       bool
	healthCheckedAt     time.Time
	healthyUntil        time.Time
	// reviewUsable records that at least one real moderation POST succeeded
	// for the current channel configuration. Unlike healthyUntil, this is not
	// a 15-minute lease: subsequent availability is governed by the normal
	// breaker and by real user reviews.
	reviewUsable        bool
	lastReviewSuccessAt time.Time
	lastLatencyMS       int
	lastError           string
	heartbeatStatus     string
	heartbeatCheckedAt  time.Time
	heartbeatLatencyMS  int
	heartbeatHTTPStatus int
	heartbeatError      string
}

type contentModerationDeepSeekHTTPError struct {
	status     int
	retryAfter time.Duration
}

func (e *contentModerationDeepSeekHTTPError) Error() string {
	return fmt.Sprintf("DeepSeek HTTP status %d", e.status)
}

func (s *ContentModerationService) deepSeekChannelState(channel ContentModerationDeepSeekChannel) *contentModerationDeepSeekChannelState {
	digest := contentModerationDeepSeekChannelDigest(channel)
	connectivityDigest := contentModerationDeepSeekConnectivityDigest(channel)
	credentialDigest := contentModerationDeepSeekCredentialDigest(channel.APIKey)
	stateKey := strings.TrimSpace(channel.ID) + "\x00" + connectivityDigest
	candidate := &contentModerationDeepSeekChannelState{
		configDigest: digest, connectivityDigest: connectivityDigest, credentialDigest: credentialDigest,
	}
	actual, _ := s.deepSeekChannelStates.LoadOrStore(stateKey, candidate)
	state, ok := actual.(*contentModerationDeepSeekChannelState)
	if !ok || state == nil {
		state = candidate
		s.deepSeekChannelStates.Store(stateKey, state)
	}
	state.mu.Lock()
	if state.configDigest != digest {
		credentialChanged := state.credentialDigest != credentialDigest
		authDisabled := state.authDisabled && !credentialChanged
		state.configDigest = digest
		state.connectivityDigest = connectivityDigest
		state.credentialDigest = credentialDigest
		state.consecutiveFailures = 0
		state.authDisabled = authDisabled
		state.cooldownUntil = time.Time{}
		state.halfOpenProbe = false
		state.healthCheckedAt = time.Time{}
		state.healthyUntil = time.Time{}
		state.reviewUsable = false
		state.lastReviewSuccessAt = time.Time{}
		state.lastLatencyMS = 0
		state.lastError = ""
		state.heartbeatStatus = ""
		state.heartbeatCheckedAt = time.Time{}
		state.heartbeatLatencyMS = 0
		state.heartbeatHTTPStatus = 0
		state.heartbeatError = ""
	}
	state.mu.Unlock()
	return state
}

func contentModerationDeepSeekCredentialDigest(apiKey string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(digest[:])
}

func contentModerationDeepSeekChannelDigest(channel ContentModerationDeepSeekChannel) string {
	keyHash := sha256.Sum256([]byte(channel.APIKey))
	canonical := strings.Join([]string{
		contentModerationRemoteProvider(channel), strings.TrimSpace(channel.ID), strings.TrimSpace(channel.BaseURL), strings.TrimSpace(channel.Model),
		strconv.Itoa(channel.TimeoutMS), hex.EncodeToString(keyHash[:]), ContentModerationDeepSeekPromptVersion,
	}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func contentModerationDeepSeekConnectivityDigest(channel ContentModerationDeepSeekChannel) string {
	canonical := strings.Join([]string{
		contentModerationRemoteProvider(channel), strings.TrimSpace(channel.ID), strings.TrimSpace(channel.BaseURL), strconv.Itoa(channel.TimeoutMS),
	}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func (state *contentModerationDeepSeekChannelState) begin(now time.Time, bypass bool) (bool, string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if bypass {
		return true, ""
	}
	if state.authDisabled {
		return false, "auth_disabled"
	}
	if now.Before(state.cooldownUntil) {
		return false, "cooldown"
	}
	if !state.cooldownUntil.IsZero() {
		if state.halfOpenProbe {
			return false, "half_open_busy"
		}
		state.halfOpenProbe = true
	}
	return true, ""
}

func (state *contentModerationDeepSeekChannelState) finish(
	now time.Time,
	latency int,
	callErr error,
	expectedConfigDigest string,
) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.configDigest != expectedConfigDigest {
		return
	}
	state.halfOpenProbe = false
	state.lastLatencyMS = latency
	if callErr == nil {
		state.consecutiveFailures = 0
		state.authDisabled = false
		state.cooldownUntil = time.Time{}
		state.healthCheckedAt = now
		state.healthyUntil = now.Add(contentModerationDeepSeekHealthTTL)
		state.lastReviewSuccessAt = now
		state.lastError = ""
		return
	}
	if errors.Is(callErr, context.Canceled) {
		return
	}
	state.lastError = trimRunes(redactContentModerationSecrets(callErr.Error()), 240)
	state.healthCheckedAt = now
	state.healthyUntil = time.Time{}
	var httpErr *contentModerationDeepSeekHTTPError
	if errors.As(callErr, &httpErr) {
		switch httpErr.status {
		case http.StatusUnauthorized, http.StatusForbidden:
			state.authDisabled = true
			state.reviewUsable = false
			state.lastReviewSuccessAt = time.Time{}
			state.cooldownUntil = time.Time{}
			return
		case http.StatusTooManyRequests, 529:
			cooldown := httpErr.retryAfter
			if cooldown <= 0 {
				cooldown = contentModerationDeepSeekRateCooldown
			}
			state.cooldownUntil = now.Add(cooldown)
			return
		}
	}
	state.consecutiveFailures++
	if state.consecutiveFailures >= contentModerationDeepSeekFailureTrip {
		state.cooldownUntil = now.Add(contentModerationDeepSeekCooldown)
	}
}

func (state *contentModerationDeepSeekChannelState) markReviewHealthy(
	now time.Time,
	expectedConfigDigest string,
) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.configDigest != expectedConfigDigest {
		return false
	}
	state.healthCheckedAt = now
	state.healthyUntil = now.Add(contentModerationDeepSeekHealthTTL)
	state.reviewUsable = true
	state.lastReviewSuccessAt = now
	state.lastError = ""
	return true
}

func (state *contentModerationDeepSeekChannelState) recordHeartbeat(
	now time.Time,
	status string,
	latency int,
	httpStatus int,
	errText string,
) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.heartbeatStatus = status
	state.heartbeatCheckedAt = now
	state.heartbeatLatencyMS = latency
	state.heartbeatHTTPStatus = httpStatus
	state.heartbeatError = trimRunes(redactContentModerationSecrets(errText), 240)
}

func (state *contentModerationDeepSeekChannelState) reviewIsUsable(now time.Time) (bool, string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.authDisabled {
		return false, "auth_disabled"
	}
	if !state.reviewUsable {
		return false, "unreviewed"
	}
	if !state.cooldownUntil.IsZero() && now.Before(state.cooldownUntil) {
		return false, "cooldown"
	}
	return true, ""
}

func (state *contentModerationDeepSeekChannelState) lastReviewSuccess() time.Time {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.lastReviewSuccessAt
}

func (state *contentModerationDeepSeekChannelState) heartbeatSnapshot() (status string, checkedAt *time.Time, latency, httpStatus int, lastError string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	status = state.heartbeatStatus
	if !state.heartbeatCheckedAt.IsZero() {
		checked := state.heartbeatCheckedAt
		checkedAt = &checked
	}
	return status, checkedAt, state.heartbeatLatencyMS, state.heartbeatHTTPStatus, state.heartbeatError
}

func (state *contentModerationDeepSeekChannelState) markCredentialUnavailable(now time.Time, credentialErr error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.authDisabled = true
	state.reviewUsable = false
	state.lastReviewSuccessAt = time.Time{}
	state.cooldownUntil = time.Time{}
	state.halfOpenProbe = false
	state.healthCheckedAt = now
	state.healthyUntil = time.Time{}
	state.lastLatencyMS = 0
	state.lastError = "credential_unavailable"
	if credentialErr != nil {
		state.lastError = trimRunes(redactContentModerationSecrets(credentialErr.Error()), 240)
	}
}

func (state *contentModerationDeepSeekChannelState) snapshot(now time.Time) (health, breaker string, checkedAt, healthyUntil, cooldownUntil *time.Time, latency int, lastError string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	health = "untested"
	if !state.healthCheckedAt.IsZero() {
		checked := state.healthCheckedAt
		checkedAt = &checked
		// `reviewUsable` is the durable startup/API-test gate. The short
		// `healthyUntil` window still describes ordinary request health so
		// existing operational status remains useful without granting Enforce
		// readiness to an untested channel.
		if state.lastError == "" && (state.reviewUsable || now.Before(state.healthyUntil)) {
			health = "reachable"
		} else {
			health = "unreachable"
		}
	}
	if !state.healthyUntil.IsZero() {
		until := state.healthyUntil
		healthyUntil = &until
	}
	breaker = "closed"
	switch {
	case state.authDisabled:
		breaker = "auth_disabled"
	case now.Before(state.cooldownUntil):
		breaker = "cooldown"
		until := state.cooldownUntil
		cooldownUntil = &until
	case !state.cooldownUntil.IsZero():
		breaker = "half_open"
	}
	return health, breaker, checkedAt, healthyUntil, cooldownUntil, state.lastLatencyMS, state.lastError
}

func (s *ContentModerationService) deepSeekHTTPClient(channel ContentModerationDeepSeekChannel) *http.Client {
	key := strings.Join([]string{channel.BaseURL, strconv.Itoa(channel.TimeoutMS)}, "\x00")
	if existing, ok := s.deepSeekHTTPClients.Load(key); ok {
		if client, ok := existing.(*http.Client); ok && client != nil {
			return client
		}
	}
	timeout := time.Duration(channel.TimeoutMS) * time.Millisecond
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	parsedBaseURL, _ := url.Parse(channel.BaseURL)
	allowLoopback := parsedBaseURL != nil && parsedBaseURL.Scheme == "http"
	client := &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: nil, ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return contentModerationDeepSeekDialContext(ctx, dialer, network, address, allowLoopback)
			},
			MaxConnsPerHost: 0, MaxIdleConns: 256, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second,
			TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	actual, _ := s.deepSeekHTTPClients.LoadOrStore(key, client)
	if shared, ok := actual.(*http.Client); ok && shared != nil {
		return shared
	}
	return client
}

func contentModerationDeepSeekDialContext(
	ctx context.Context,
	dialer *net.Dialer,
	network string,
	address string,
	allowLoopback bool,
) (net.Conn, error) {
	if dialer == nil {
		return nil, errors.New("DeepSeek dialer is unavailable")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid DeepSeek upstream address: %w", err)
	}
	var ips []net.IP
	if literal := net.ParseIP(strings.Trim(host, "[]")); literal != nil {
		ips = []net.IP{literal}
	} else {
		resolved, resolveErr := net.DefaultResolver.LookupIPAddr(ctx, host)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve DeepSeek upstream: %w", resolveErr)
		}
		for _, item := range resolved {
			ips = append(ips, item.IP)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("DeepSeek upstream resolved no addresses")
	}
	for _, ip := range ips {
		if contentModerationDeepSeekPublicIP(ip) || (allowLoopback && ip.IsLoopback()) {
			continue
		}
		return nil, errors.New("DeepSeek upstream resolved to a non-public address")
	}
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("connect DeepSeek upstream: %w", lastErr)
}

func contentModerationDeepSeekChatURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid DeepSeek Base URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(strings.ToLower(path), "/chat/completions"):
	case strings.HasSuffix(strings.ToLower(path), "/v1"):
		path += "/chat/completions"
	default:
		path += "/v1/chat/completions"
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.String(), nil
}

func contentModerationDeepSeekPrompt() (string, error) {
	asset, err := contentmoderationassets.Load(contentmoderationassets.DeepSeekV4FlashAuditV3)
	if err != nil {
		return "", err
	}
	prompt := strings.TrimSpace(asset.SystemPrompt)
	if prompt == "" {
		return "", errors.New("DeepSeek moderation system prompt is empty")
	}
	return prompt, nil
}

func buildContentModerationDeepSeekPayload(channel ContentModerationDeepSeekChannel, input contentModerationSecondLayerInput) (map[string]any, error) {
	prompt, err := contentModerationDeepSeekPrompt()
	if err != nil {
		return nil, err
	}
	dataFields := map[string]any{"content": input.Evidence.Text}
	if len(input.MatchedIndicators) > 0 {
		dataFields["matched_risk_indicators"] = append([]string(nil), input.MatchedIndicators...)
	}
	if input.ChunkCount > 1 {
		dataFields["chunk"] = map[string]int{"index": input.ChunkIndex + 1, "total": input.ChunkCount}
	}
	data, err := json.Marshal(dataFields)
	if err != nil {
		return nil, err
	}
	trusted, err := json.Marshal(map[string]string{
		"context_class": input.Fragment.ContextClass,
		"role":          input.Fragment.Role,
		"kind":          input.Fragment.Kind,
	})
	if err != nil {
		return nil, err
	}
	message := "<trusted_context>" + string(trusted) + "</trusted_context>\n<user_input>" + string(data) + "</user_input>"
	return map[string]any{
		"model": channel.Model,
		"messages": []map[string]string{
			{"role": "system", "content": prompt},
			{"role": "user", "content": message},
		},
		"thinking":        map[string]string{"type": "disabled"},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0,
		"max_tokens":      96,
		"stream":          false,
	}, nil
}

func (s *ContentModerationService) callContentModerationDeepSeekChannel(
	ctx context.Context,
	channel ContentModerationDeepSeekChannel,
	input contentModerationSecondLayerInput,
	bypassBreaker bool,
) (contentModerationSecondLayerResult, ContentModerationReviewAttempt, error) {
	attempt := ContentModerationReviewAttempt{Reviewer: "deepseek", Provider: ContentModerationRemoteProviderDeepSeek, ChannelID: channel.ID, ChannelName: channel.Name, Model: channel.Model}
	stampContentModerationReviewAttemptChunk(&attempt, input)
	state := s.deepSeekChannelState(channel)
	configDigest := contentModerationDeepSeekChannelDigest(channel)
	if allowed, reason := state.begin(time.Now(), bypassBreaker); !allowed {
		attempt.Outcome = "skipped"
		attempt.Error = reason
		return contentModerationSecondLayerResult{}, attempt, fmt.Errorf("DeepSeek channel %s is %s", channel.ID, reason)
	}
	started := time.Now()
	finish := func(callErr error) {
		latency := int(time.Since(started).Milliseconds())
		attempt.LatencyMS = latency
		state.finish(time.Now(), latency, callErr, configDigest)
	}
	payloadValue, err := buildContentModerationDeepSeekPayload(channel, input)
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = "payload"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = "payload"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	endpoint, err := contentModerationDeepSeekChatURL(channel.BaseURL)
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = "base_url"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(channel.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = "request"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+channel.APIKey)
	resp, err := s.deepSeekHTTPClient(channel).Do(req)
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = contentModerationDeepSeekCallErrorKind(ctx, requestCtx, err, "network")
		return contentModerationSecondLayerResult{}, attempt, err
	}
	defer func() { _ = resp.Body.Close() }()
	attempt.HTTPStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, contentModerationDeepSeekMaxResponseBytes))
		httpErr := &contentModerationDeepSeekHTTPError{status: resp.StatusCode, retryAfter: contentModerationDeepSeekRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
		finish(httpErr)
		attempt.Outcome = "error"
		attempt.Error = "http_" + strconv.Itoa(resp.StatusCode)
		return contentModerationSecondLayerResult{}, attempt, httpErr
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, contentModerationDeepSeekMaxResponseBytes+1))
	if err != nil || len(body) > contentModerationDeepSeekMaxResponseBytes {
		if err == nil {
			err = errors.New("DeepSeek response too large")
			attempt.Error = "response_too_large"
		} else {
			attempt.Error = contentModerationDeepSeekCallErrorKind(ctx, requestCtx, err, "response_read")
		}
		finish(err)
		attempt.Outcome = "error"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	result, err := parseContentModerationDeepSeekResponse(body)
	finish(err)
	if err != nil {
		attempt.Outcome = "error"
		attempt.Error = "invalid_json"
		return contentModerationSecondLayerResult{}, attempt, fmt.Errorf("%w: %v", errContentModerationSecondLayerParse, err)
	}
	result.Profile = "deepseek_v4_flash"
	result.PromptVersion = ContentModerationDeepSeekPromptVersion
	if result.ParserStatus == "" {
		result.ParserStatus = "parsed"
	}
	result.EvidenceMode = input.Evidence.Mode
	result.EvidenceTruncated = input.Evidence.Truncated
	result.EndpointID = channel.ID
	result.KeywordTier = input.KeywordTier
	result.KeywordRuleID = input.KeywordRuleID
	attempt.Outcome = "success"
	attempt.Confidence = result.Confidence
	switch result.normalizedDisposition() {
	case ContentModerationReviewDispositionViolation:
		attempt.Verdict = "violation"
	case ContentModerationReviewDispositionRestricted:
		attempt.Verdict = "restricted"
	default:
		attempt.Verdict = "safe"
	}
	return result, attempt, nil
}

func contentModerationDeepSeekCallErrorKind(
	parentCtx context.Context,
	requestCtx context.Context,
	err error,
	fallback string,
) string {
	if parentCtx != nil && errors.Is(parentCtx.Err(), context.Canceled) {
		return "canceled"
	}
	if requestCtx != nil && errors.Is(requestCtx.Err(), context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	timedOut := (parentCtx != nil && errors.Is(parentCtx.Err(), context.DeadlineExceeded)) ||
		(requestCtx != nil && errors.Is(requestCtx.Err(), context.DeadlineExceeded)) ||
		errors.Is(err, context.DeadlineExceeded)
	if !timedOut {
		var netErr net.Error
		timedOut = errors.As(err, &netErr) && netErr.Timeout()
	}
	if timedOut {
		if fallback == "response_read" {
			return "response_read_timeout"
		}
		return "timeout"
	}
	return fallback
}

func contentModerationDeepSeekAttemptRetryable(attempt ContentModerationReviewAttempt) bool {
	switch attempt.Error {
	case "network", "timeout", "response_read", "response_read_timeout":
		return true
	}
	if !strings.HasPrefix(attempt.Error, "http_") {
		return false
	}
	status, err := strconv.Atoi(strings.TrimPrefix(attempt.Error, "http_"))
	return err == nil && (status == http.StatusRequestTimeout ||
		(status >= http.StatusInternalServerError && status <= 599 && status != 529))
}

func contentModerationDeepSeekHasRetryBudget(ctx context.Context, channel ContentModerationDeepSeekChannel) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	minimum := contentModerationDeepSeekRetryMinBudget
	channelTimeout := time.Duration(channel.TimeoutMS) * time.Millisecond
	if channelTimeout > 0 && channelTimeout < minimum {
		minimum = channelTimeout / 2
	}
	if minimum <= 0 {
		return false
	}
	return time.Until(deadline) >= minimum
}

func (s *ContentModerationService) callContentModerationDeepSeekChannelWithRetry(
	ctx context.Context,
	channel ContentModerationDeepSeekChannel,
	input contentModerationSecondLayerInput,
) (contentModerationSecondLayerResult, []ContentModerationReviewAttempt, error) {
	result, attempt, err := s.callContentModerationDeepSeekChannel(ctx, channel, input, false)
	attempts := []ContentModerationReviewAttempt{attempt}
	if err == nil || !contentModerationDeepSeekAttemptRetryable(attempt) ||
		!contentModerationDeepSeekHasRetryBudget(ctx, channel) {
		return result, attempts, err
	}
	retryResult, retryAttempt, retryErr := s.callContentModerationDeepSeekChannel(ctx, channel, input, false)
	attempts = append(attempts, retryAttempt)
	if retryErr == nil {
		return retryResult, attempts, nil
	}
	return contentModerationSecondLayerResult{}, attempts, errors.Join(err, retryErr)
}

func contentModerationDeepSeekRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		return deadline.Sub(now)
	}
	return 0
}

func parseContentModerationDeepSeekResponse(body []byte) (contentModerationSecondLayerResult, error) {
	var envelope struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string          `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Choices) == 0 {
		return contentModerationSecondLayerResult{}, errors.New("invalid DeepSeek response envelope")
	}
	if strings.EqualFold(strings.TrimSpace(envelope.Choices[0].FinishReason), "length") {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek response was truncated")
	}
	reasoning := bytes.TrimSpace(envelope.Choices[0].Message.ReasoningContent)
	if len(reasoning) > 0 && !bytes.Equal(reasoning, []byte("null")) && !bytes.Equal(reasoning, []byte(`""`)) {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek returned reasoning_content")
	}
	content := strings.TrimSpace(envelope.Choices[0].Message.Content)
	if content == "" {
		return contentModerationSecondLayerResult{}, errors.New("empty DeepSeek response content")
	}
	var decision struct {
		Disposition *string  `json:"disposition"`
		Confidence  *float64 `json:"confidence"`
		Category    *string  `json:"category"`
		Reason      *string  `json:"reason"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return contentModerationSecondLayerResult{}, fmt.Errorf("invalid DeepSeek decision JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek decision contains trailing data")
	}
	if decision.Disposition == nil || decision.Confidence == nil || decision.Category == nil || decision.Reason == nil {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek decision is missing required fields")
	}
	confidence := *decision.Confidence
	category := strings.TrimSpace(*decision.Category)
	reason := strings.TrimSpace(*decision.Reason)
	if confidence < 0 || confidence > 1 {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek confidence is out of range")
	}
	if _, ok := contentModerationDeepSeekCategories[category]; !ok {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek category is unknown")
	}
	disposition := strings.ToLower(strings.TrimSpace(*decision.Disposition))
	parserStatus := ""
	switch disposition {
	case ContentModerationReviewDispositionAllow:
		if category != "safe" {
			return contentModerationSecondLayerResult{}, errors.New("allow decision has an invalid category or confidence")
		}
		if confidence >= DefaultContentModerationDeepSeekThreshold {
			// Some compatible reviewers use confidence as certainty in the
			// selected allow verdict. The disposition and category still agree on
			// safe, so convert that value into the risk-score convention used by
			// the rest of the moderation pipeline. Risk-bearing mismatches remain
			// fail-closed below.
			confidence = 1 - confidence
			parserStatus = contentModerationParserStatusNormalizedAllowConfidence
		}
	case ContentModerationReviewDispositionRestricted:
		if category != ContentModerationRestrictedCategory || confidence < DefaultContentModerationDeepSeekThreshold {
			return contentModerationSecondLayerResult{}, errors.New("restricted decision has an invalid category or confidence")
		}
	case ContentModerationReviewDispositionViolation:
		if category == "safe" || category == ContentModerationRestrictedCategory || confidence < DefaultContentModerationDeepSeekThreshold {
			return contentModerationSecondLayerResult{}, errors.New("violation decision has an invalid category or confidence")
		}
	default:
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek disposition is unknown")
	}
	// The prompt requires a short reason, but a provider can still return a
	// longer valid JSON response. Keep the verdict and bound the audit field
	// after redaction instead of turning this nonessential field into a parse
	// failure.
	reason = redactContentModerationAuditReason(reason)
	blocked := disposition != ContentModerationReviewDispositionAllow
	return contentModerationSecondLayerResult{
		Blocked: blocked, Disposition: disposition,
		Category: category, Confidence: confidence, Reason: reason, ParserStatus: parserStatus,
	}, nil
}

func (s *ContentModerationService) scanContentModerationDeepSeek(
	ctx context.Context,
	cfg *ContentModerationConfig,
	input contentModerationSecondLayerInput,
) (contentModerationSecondLayerResult, bool, error) {
	return s.scanContentModerationRemotePool(ctx, cfg, input)
}

func (s *ContentModerationService) testDeepSeekChannelConnectivity(ctx context.Context, channelID string) (*TestContentModerationDeepSeekChannelResult, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return nil, errors.New("DeepSeek channel ID is required")
	}
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	var channel *ContentModerationDeepSeekChannel
	for i := range cfg.DeepSeekChannels {
		if cfg.DeepSeekChannels[i].ID == channelID {
			channel = &cfg.DeepSeekChannels[i]
			break
		}
	}
	if channel == nil {
		return nil, errors.New("DeepSeek channel not found")
	}
	if strings.TrimSpace(channel.APIKey) == "" {
		return nil, errors.New("DeepSeek channel API key is not configured")
	}
	return s.probeDeepSeekChannelConnectivity(ctx, *channel), nil
}

func (s *ContentModerationService) probeDeepSeekChannelConnectivity(
	ctx context.Context,
	channel ContentModerationDeepSeekChannel,
) *TestContentModerationDeepSeekChannelResult {
	if err := ctx.Err(); err != nil {
		return contentModerationDeepSeekCanceledConnectivityResult(channel.ID, err)
	}
	probeTimeout := time.Duration(channel.TimeoutMS) * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < probeTimeout {
			probeTimeout = remaining
		}
	}
	if probeTimeout <= 0 {
		return contentModerationDeepSeekCanceledConnectivityResult(channel.ID, context.DeadlineExceeded)
	}
	digest := contentModerationDeepSeekConnectivityDigest(channel)
	resultCh := s.deepSeekProbeFlights.DoChan(digest, func() (any, error) {
		probeCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		return s.probeDeepSeekChannelConnectivityOnce(probeCtx, channel), nil
	})
	select {
	case <-ctx.Done():
		return contentModerationDeepSeekCanceledConnectivityResult(channel.ID, ctx.Err())
	case shared := <-resultCh:
		if shared.Err != nil {
			return &TestContentModerationDeepSeekChannelResult{
				ChannelID: channel.ID,
				Error:     trimRunes(redactContentModerationSecrets(shared.Err.Error()), 240),
			}
		}
		result, ok := shared.Val.(*TestContentModerationDeepSeekChannelResult)
		if !ok || result == nil {
			return &TestContentModerationDeepSeekChannelResult{
				ChannelID: channel.ID,
				Error:     "invalid shared connectivity result",
			}
		}
		return result
	}
}

func contentModerationDeepSeekCanceledConnectivityResult(
	channelID string,
	err error,
) *TestContentModerationDeepSeekChannelResult {
	if err == nil {
		err = context.Canceled
	}
	return &TestContentModerationDeepSeekChannelResult{
		ChannelID: channelID,
		Error:     trimRunes(redactContentModerationSecrets(err.Error()), 240),
	}
}

func (s *ContentModerationService) probeDeepSeekChannelConnectivityOnce(
	ctx context.Context,
	channel ContentModerationDeepSeekChannel,
) *TestContentModerationDeepSeekChannelResult {
	started := time.Now()
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(channel.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodHead, channel.BaseURL, nil)
	if err != nil {
		now := time.Now()
		return &TestContentModerationDeepSeekChannelResult{
			ChannelID: channel.ID, LatencyMS: int(time.Since(started).Milliseconds()),
			Error: trimRunes(redactContentModerationSecrets(err.Error()), 240), CheckedAt: &now,
		}
	}
	req.Header.Set("Authorization", "Bearer "+channel.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, pingErr := s.deepSeekHTTPClient(channel).Do(req)
	latency := int(time.Since(started).Milliseconds())
	reachable := pingErr == nil
	httpStatus := 0
	if resp != nil {
		httpStatus = resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
	}
	now := time.Now()
	result := &TestContentModerationDeepSeekChannelResult{
		ChannelID: channel.ID, Reachable: reachable, HealthValid: reachable,
		LatencyMS: latency, HTTPStatus: httpStatus, CheckedAt: &now,
	}
	if pingErr != nil {
		result.Error = trimRunes(redactContentModerationSecrets(pingErr.Error()), 240)
	}
	return result
}

func (s *ContentModerationService) contentModerationDeepSeekChannelView(channel ContentModerationDeepSeekChannel) ContentModerationDeepSeekChannelView {
	view := contentModerationDeepSeekChannelViews([]ContentModerationDeepSeekChannel{channel})[0]
	if !channel.Enabled {
		return view
	}
	state := s.deepSeekChannelState(channel)
	health, breaker, checkedAt, _, cooldownUntil, latency, lastError := state.snapshot(time.Now())
	view.HealthStatus = health
	view.BreakerStatus = breaker
	view.LastHealthCheckedAt = checkedAt
	view.CooldownUntil = cooldownUntil
	view.LastLatencyMS = latency
	view.LastError = lastError
	heartbeatStatus, heartbeatAt, heartbeatLatency, heartbeatHTTPStatus, heartbeatError := state.heartbeatSnapshot()
	if heartbeatStatus == "" {
		heartbeatStatus = "untested"
	}
	view.HeartbeatStatus = heartbeatStatus
	view.LastHeartbeatAt = heartbeatAt
	view.HeartbeatLatencyMS = heartbeatLatency
	view.HeartbeatHTTPStatus = heartbeatHTTPStatus
	view.HeartbeatError = heartbeatError
	return view
}

func (s *ContentModerationService) deepSeekChannelReviewReady(channel ContentModerationDeepSeekChannel, now time.Time) bool {
	state := s.deepSeekChannelState(channel)
	usable, _ := state.reviewIsUsable(now)
	if !usable {
		return false
	}
	_, breaker, _, _, _, _, _ := state.snapshot(now)
	return breaker == "closed" || breaker == "half_open"
}

func (s *ContentModerationService) hasReachableDeepSeekChannel(cfg *ContentModerationConfig, now time.Time) bool {
	if cfg == nil || !contentModerationRemoteReviewersEnabled(cfg) {
		return false
	}
	for _, channel := range cfg.DeepSeekChannels {
		if !channel.Enabled {
			continue
		}
		if strings.TrimSpace(channel.APIKey) == "" {
			continue
		}
		if s.deepSeekChannelReviewReady(channel, now) {
			return true
		}
	}
	return false
}

func (s *ContentModerationService) contentModerationConfigWithReachableDeepSeekFirst(
	cfg *ContentModerationConfig,
	now time.Time,
) *ContentModerationConfig {
	if cfg == nil || cfg.RemoteReviewersVersion >= 1 {
		// Provider order is an explicit policy (DS -> Qwen -> GLM -> MiMo). A
		// health probe must never rewrite that order or turn failover into
		// round-robin scheduling for the persisted reviewer pool.
		return cfg
	}
	// Legacy in-memory callers used the last review-healthy channel as a
	// preferred backup. Keep that behavior isolated to version-zero configs;
	// the provider pool above must always start with DeepSeek and then follow
	// its fixed policy order.
	clone := cloneContentModerationConfig(cfg)
	for index, channel := range clone.DeepSeekChannels {
		if !channel.Enabled || strings.TrimSpace(channel.APIKey) == "" ||
			!s.deepSeekChannelReviewReady(channel, now) {
			continue
		}
		if index > 0 {
			preferred := clone.DeepSeekChannels[index]
			minOrder := preferred.Order
			for _, candidate := range clone.DeepSeekChannels {
				if candidate.Order < minOrder {
					minOrder = candidate.Order
				}
			}
			preferred.Order = minOrder - 1
			copy(clone.DeepSeekChannels[1:index+1], clone.DeepSeekChannels[0:index])
			clone.DeepSeekChannels[0] = preferred
		}
		break
	}
	return clone
}

func contentModerationDeepSeekReviewProbeInput() contentModerationSecondLayerInput {
	const text = "Sub2API content moderation reviewer health check."
	fragment, _ := newContentModerationFragment("user", "text", "health_check", text)
	return contentModerationSecondLayerInput{
		Fragment: fragment,
		Evidence: moderationEvidence{Text: text, Mode: "health_check"},
	}
}

func (s *ContentModerationService) probeDeepSeekChannelReview(
	ctx context.Context,
	channel ContentModerationDeepSeekChannel,
	retry bool,
) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	probeTimeout := 2 * time.Duration(channel.TimeoutMS) * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < probeTimeout {
			probeTimeout = remaining
		}
	}
	if probeTimeout <= 0 {
		return false
	}
	// Startup is the only automatic caller. Keep the provider request attached
	// to the worker context so CloseContentModerationRuntime cancels the actual
	// network call, not merely the goroutine waiting for it.
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var attempts []ContentModerationReviewAttempt
	var err error
	if retry && contentModerationRemoteProvider(channel) == ContentModerationRemoteProviderDeepSeek {
		_, attempts, err = s.callContentModerationDeepSeekChannelWithRetry(
			probeCtx, channel, contentModerationDeepSeekReviewProbeInput(),
		)
	} else {
		var attempt ContentModerationReviewAttempt
		if contentModerationRemoteProvider(channel) == ContentModerationRemoteProviderDeepSeek {
			_, attempt, err = s.callContentModerationDeepSeekChannel(
				probeCtx, channel, contentModerationDeepSeekReviewProbeInput(), false,
			)
		} else {
			_, attempt, err = s.callContentModerationRemoteChannel(
				probeCtx, channel, contentModerationDeepSeekReviewProbeInput(), false,
			)
		}
		attempts = []ContentModerationReviewAttempt{attempt}
	}
	s.recordContentModerationReviewAttempts(attempts, err)
	if err != nil {
		return false
	}
	// Only the dedicated startup/API-usability probe may establish the Enforce
	// readiness gate. Normal user reviews do not silently enable a new channel.
	s.deepSeekChannelState(channel).markReviewHealthy(time.Now(), contentModerationDeepSeekChannelDigest(channel))
	return true
}

func (s *ContentModerationService) ensureContentModerationSecondLayerEnforceReadiness(
	_ context.Context,
	cfg *ContentModerationConfig,
	now time.Time,
) (bool, string) {
	// This path is intentionally observational. Paid review probes are allowed
	// only in the process-start worker and the explicit admin /test-api action.
	return s.contentModerationSecondLayerEnforceReadiness(cfg, now)
}
