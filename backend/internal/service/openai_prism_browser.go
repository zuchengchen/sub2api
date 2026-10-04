package service

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const prismBrowserMaxResponseBytes = 2 << 20

const prismBrowserAttemptsKey = "prism_browser_attempts"

const prismBrowserUpstreamEndpoint = "/prism-adapter/v1/responses"

var errPrismBrowserHTTPFallback = errors.New("prism browser fallback to HTTP")

func prismBrowserResponsesURL(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return ""
	}
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	return base + "/responses"
}

func MarkPrismBrowserAttempt(c *gin.Context, accountID int64) {
	value, _ := c.Get(prismBrowserAttemptsKey)
	ids, _ := value.(map[int64]bool)
	if ids == nil {
		ids = make(map[int64]bool)
	}
	ids[accountID] = true
	c.Set(prismBrowserAttemptsKey, ids)
}

func IsPrismBrowserAttempt(c *gin.Context, accountID int64) bool {
	value, _ := c.Get(prismBrowserAttemptsKey)
	ids, _ := value.(map[int64]bool)
	return ids[accountID]
}

func ClearPrismBrowserAttempt(c *gin.Context, accountID int64) {
	if c == nil {
		return
	}
	value, _ := c.Get(prismBrowserAttemptsKey)
	ids, _ := value.(map[int64]bool)
	if ids == nil {
		return
	}
	delete(ids, accountID)
	c.Set(prismBrowserAttemptsKey, ids)
}

func prismBrowserTerminal(body []byte, model string, stream bool) (string, error) {
	terminal := body
	var itemEvents []gjson.Result
	var detailEvents []gjson.Result
	if stream {
		terminal = nil
		for _, line := range bytes.Split(body, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if !gjson.ValidBytes(data) {
				return "", errors.New("prism adapter returned invalid SSE JSON")
			}
			if terminal != nil {
				return "", errors.New("prism adapter returned events after its terminal response")
			}
			switch gjson.GetBytes(data, "type").String() {
			case "response.created", "response.in_progress":
			case "response.content_part.added", "response.content_part.done", "response.output_text.done",
				"response.function_call_arguments.done", "response.custom_tool_call_input.done":
				detailEvents = append(detailEvents, gjson.ParseBytes(data))
			case "response.output_item.added", "response.output_item.done":
				itemEvents = append(itemEvents, gjson.ParseBytes(data))
			case "response.completed":
				if terminal != nil {
					return "", errors.New("prism adapter returned repeated terminal events")
				}
				terminal = []byte(gjson.GetBytes(data, "response").Raw)
			default:
				return "", errors.New("prism adapter returned unsupported SSE event")
			}
		}
	}
	reported := strings.TrimSpace(gjson.GetBytes(terminal, "model").String())
	if !gjson.ValidBytes(terminal) || gjson.GetBytes(terminal, "status").String() != "completed" ||
		(reported != "" && reported != model) ||
		gjson.GetBytes(terminal, "id").String() == "" {
		return "", errors.New("prism adapter returned an invalid terminal response")
	}
	output := gjson.GetBytes(terminal, "output")
	if !output.IsArray() || len(output.Array()) == 0 {
		return "", errors.New("prism adapter returned no output items")
	}
	seen := make(map[string]bool)
	items := output.Array()
	for _, item := range items {
		switch item.Get("type").String() {
		case "", "message":
			if item.Get("content.0.text").String() == "" {
				return "", errors.New("prism adapter returned an empty message")
			}
		case "function_call", "custom_tool_call":
			id := item.Get("call_id").String()
			if id == "" || seen[id] || item.Get("id").String() == "" || item.Get("name").String() == "" || item.Get("status").String() != "completed" {
				return "", errors.New("prism adapter returned an invalid tool identity")
			}
			seen[id] = true
			if item.Get("type").String() == "function_call" {
				args := item.Get("arguments")
				if args.Type != gjson.String || !gjson.Valid(args.String()) || !gjson.Parse(args.String()).IsObject() {
					return "", errors.New("prism adapter returned invalid function arguments")
				}
			} else if item.Get("input").Type != gjson.String {
				return "", errors.New("prism adapter returned invalid custom input")
			}
		default:
			return "", errors.New("prism adapter returned an unsupported output item")
		}
	}
	added, completed := make(map[int64]bool), make(map[int64]bool)
	for _, event := range itemEvents {
		index := event.Get("output_index")
		if index.Type != gjson.Number || index.Int() < 0 || index.Float() != float64(index.Int()) || index.Int() >= int64(len(items)) {
			return "", errors.New("prism adapter returned an invalid output index")
		}
		item, final := event.Get("item"), items[index.Int()]
		if item.Get("status").String() != "completed" && item.Get("status").String() != "in_progress" {
			return "", errors.New("prism adapter returned an invalid item status")
		}
		for _, key := range []string{"id", "type", "call_id", "name", "namespace"} {
			if item.Get(key).String() != final.Get(key).String() {
				return "", errors.New("prism adapter returned mismatched item events")
			}
		}
		if event.Get("type").String() == "response.output_item.done" {
			if !added[index.Int()] || completed[index.Int()] {
				return "", errors.New("prism adapter returned unpaired item events")
			}
			completed[index.Int()] = true
			left, _ := json.Marshal(item.Value())
			right, _ := json.Marshal(final.Value())
			if !bytes.Equal(left, right) {
				return "", errors.New("prism adapter returned conflicting completed items")
			}
		} else {
			if added[index.Int()] || item.Get("status").String() != "in_progress" || item.Get("arguments").String() != "" || item.Get("input").String() != "" || len(item.Get("content").Array()) != 0 {
				return "", errors.New("prism adapter returned an invalid added item")
			}
			added[index.Int()] = true
		}
	}
	for index, item := range items {
		if added[int64(index)] != completed[int64(index)] || (stream && item.Get("call_id").String() != "" && !completed[int64(index)]) {
			return "", errors.New("prism adapter omitted a completed output item")
		}
	}
	details := make(map[string]bool)
	for _, event := range detailEvents {
		index, kind := event.Get("output_index"), event.Get("type").String()
		if index.Type != gjson.Number || index.Int() < 0 || index.Float() != float64(index.Int()) || index.Int() >= int64(len(items)) {
			return "", errors.New("prism adapter returned an invalid detail index")
		}
		item := items[index.Int()]
		if event.Get("item_id").String() != item.Get("id").String() || !completed[index.Int()] {
			return "", errors.New("prism adapter returned a foreign item detail")
		}
		key := fmt.Sprintf("%s:%d:%d", kind, index.Int(), event.Get("content_index").Int())
		if details[key] {
			return "", errors.New("prism adapter repeated an item detail")
		}
		details[key] = true
		switch kind {
		case "response.function_call_arguments.done":
			if item.Get("type").String() != "function_call" || event.Get("arguments").Type != gjson.String || event.Get("arguments").String() != item.Get("arguments").String() {
				return "", errors.New("prism adapter returned conflicting function arguments")
			}
		case "response.custom_tool_call_input.done":
			if item.Get("type").String() != "custom_tool_call" || event.Get("input").Type != gjson.String || event.Get("input").String() != item.Get("input").String() {
				return "", errors.New("prism adapter returned conflicting custom input")
			}
		default:
			contentIndex := event.Get("content_index")
			content := item.Get("content").Array()
			if item.Get("type").String() != "message" || contentIndex.Type != gjson.Number || contentIndex.Int() < 0 || contentIndex.Float() != float64(contentIndex.Int()) || contentIndex.Int() >= int64(len(content)) {
				return "", errors.New("prism adapter returned an invalid content index")
			}
			part := content[contentIndex.Int()]
			if (kind == "response.output_text.done" && event.Get("text").String() != part.Get("text").String()) ||
				(kind == "response.content_part.done" && event.Get("part.text").String() != part.Get("text").String()) ||
				(kind == "response.content_part.added" && event.Get("part.text").String() != "") {
				return "", errors.New("prism adapter returned conflicting text content")
			}
		}
	}
	return gjson.GetBytes(terminal, "id").String(), nil
}

func prismBrowserAdapterURL(baseURL string) (string, error) {
	parsed, err := url.Parse(prismBrowserResponsesURL(baseURL))
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", errors.New("prism adapter must use a local HTTP endpoint")
	}
	if ip := net.ParseIP(parsed.Hostname()); ip == nil || (!ip.Equal(net.ParseIP("127.0.0.1")) && !ip.Equal(net.IPv6loopback)) {
		return "", errors.New("prism adapter must bind to a numeric loopback address")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 || parsed.Path != "/v1/responses" {
		return "", errors.New("invalid Prism adapter endpoint")
	}
	return parsed.String(), nil
}

func prismBrowserAdapterMisconfigured(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
		return true
	}
	return false
}

func prismBrowserAdapterErrorMessage(status int, body []byte) string {
	code := strings.TrimSpace(gjson.GetBytes(body, "error.type").String())
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	switch {
	case code != "" && message != "":
		return fmt.Sprintf("Prism adapter returned HTTP %d (%s): %s", status, code, truncateString(message, 300))
	case code != "":
		return fmt.Sprintf("Prism adapter returned HTTP %d (%s)", status, code)
	default:
		return fmt.Sprintf("Prism adapter returned HTTP %d", status)
	}
}

func prismBrowserForwardError(status int, body []byte) error {
	code := gjson.GetBytes(body, "error.type").String()
	switch code {
	case "tools_disabled", "unsupported_model", "unsupported_request", "unsupported_reasoning",
		"unsupported_input", "unsupported_tool_model", "unsupported_tool", "invalid_tools",
		"invalid_tool_choice", "invalid_tool_payload", "unsupported_reasoning_history",
		"model_unavailable", "reasoning_unavailable", "model_catalog_unavailable",
		"pending_turn", "prism_busy":
		return fmt.Errorf("prism adapter returned HTTP %d (%s)", status, code)
	default:
		return fmt.Errorf("prism adapter returned HTTP %d", status)
	}
}

func prismBrowserShouldHTTPFallback(err error, status int, body []byte) bool {
	if err != nil || status == 0 {
		return true
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	if prismBrowserAdapterMisconfigured(status) {
		return true
	}
	switch gjson.GetBytes(body, "error.type").String() {
	case "prism_busy", "model_unavailable", "reasoning_unavailable", "model_catalog_unavailable",
		"tools_disabled", "resource_pressure", "credential_rotation", "start_not_sent",
		"unsupported_reasoning_history":
		return true
	}
	return false
}

func prismBrowserUnsupportedClientRequest(c *gin.Context, body []byte, requestedModel string) string {
	if isOpenAIResponsesCompactPath(c) || prismBrowserCompactSpelling(requestedModel) {
		return "Prism adapter does not support responses/compact"
	}
	if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
		return "Prism adapter does not support previous_response_id"
	}
	if prismBrowserRequestHasImages(body) {
		return "Prism adapter does not support images"
	}
	return ""
}

func prismBrowserRequestHasStructuredOutput(body []byte) bool {
	return prismBrowserFormatRequiresStructuredOutput(gjson.GetBytes(body, "text.format")) ||
		prismBrowserFormatRequiresStructuredOutput(gjson.GetBytes(body, "response_format"))
}

func prismBrowserFormatRequiresStructuredOutput(format gjson.Result) bool {
	if !format.Exists() || format.Type == gjson.Null {
		return false
	}
	if !format.IsObject() {
		return true
	}
	kind := strings.TrimSpace(format.Get("type").String())
	return kind != "" && !strings.EqualFold(kind, "text")
}

func prismBrowserRequestHasEncryptedReasoning(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			if prismBrowserItemHasEncryptedContent(item) {
				return true
			}
		}
		return false
	}
	if input.IsObject() {
		return prismBrowserItemHasEncryptedContent(input)
	}
	return false
}

func prismBrowserItemHasEncryptedContent(item gjson.Result) bool {
	raw := item.Get("encrypted_content")
	if !raw.Exists() || raw.Type == gjson.Null {
		return false
	}
	if raw.Type == gjson.String {
		return strings.TrimSpace(raw.String()) != ""
	}
	return true
}

func prismBrowserRequestHasImages(body []byte) bool {
	if gjson.GetBytes(body, "image").Exists() {
		return true
	}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if strings.Contains(strings.ToLower(item.Get("type").String()), "image") {
			return true
		}
		content := item.Get("content")
		if content.Type == gjson.String {
			continue
		}
		for _, part := range content.Array() {
			kind := strings.ToLower(part.Get("type").String())
			if kind == "input_image" || kind == "image_url" || strings.Contains(kind, "image") {
				return true
			}
		}
	}
	return false
}

func prismBrowserFoldEffort(raw string) string {
	folded := normalizeOpenAIReasoningEffort(raw)
	switch folded {
	case "low", "medium", "high", "xhigh":
		return folded
	default:
		if strings.TrimSpace(raw) == "" {
			return "medium"
		}
		return "medium"
	}
}

func prismBrowserPrepareAdapterBody(account *Account, body []byte) ([]byte, string, string, error) {
	original := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	mapped := original
	if account != nil {
		mapped = account.GetMappedModel(original)
	}
	canonical := prismBrowserCanonicalModel(mapped)
	if canonical == "" {
		canonical = "gpt-6.1-sol"
	}
	prepared, err := sjson.SetBytes(body, "model", canonical)
	if err != nil {
		return nil, original, "", err
	}
	effort := prismBrowserFoldEffort(gjson.GetBytes(prepared, "reasoning.effort").String())
	if gjson.GetBytes(prepared, "reasoning.effort").String() == "" {
		if raw := gjson.GetBytes(prepared, "reasoning_effort").String(); raw != "" {
			effort = prismBrowserFoldEffort(raw)
		} else if derived := extractOpenAIReasoningEffortFromBody(body, original, mapped); derived != nil {
			effort = prismBrowserFoldEffort(*derived)
		}
	}
	prepared, err = sjson.SetBytes(prepared, "reasoning.effort", effort)
	if err != nil {
		return nil, original, "", err
	}
	return prepared, original, canonical, nil
}

func prismBrowserTerminalOutputText(response []byte, stream bool) string {
	terminal := response
	if stream {
		for _, line := range bytes.Split(response, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if gjson.GetBytes(data, "type").String() == "response.completed" {
				terminal = []byte(gjson.GetBytes(data, "response").Raw)
			}
		}
	}
	var b strings.Builder
	for _, item := range gjson.GetBytes(terminal, "output").Array() {
		switch item.Get("type").String() {
		case "", "message":
			for _, part := range item.Get("content").Array() {
				b.WriteString(part.Get("text").String())
			}
		case "function_call":
			b.WriteString(item.Get("name").String())
			b.WriteString(item.Get("arguments").String())
		case "custom_tool_call":
			b.WriteString(item.Get("name").String())
			b.WriteString(item.Get("input").String())
		}
	}
	return b.String()
}

func (s *OpenAIGatewayService) forwardPrismBrowser(ctx context.Context, c *gin.Context, account *Account, body []byte, started time.Time) (*OpenAIForwardResult, error) {
	MarkPrismBrowserAttempt(c, account.ID)
	defer func() {
		if c.Writer.Written() {
			MarkResponseCommitted(c)
		}
	}()
	writeError := func(status int, raw []byte) {
		committed := StopOpenAICompactSSEKeepaliveCommitted(c)
		if committed || c.Writer.Written() {
			code := gjson.GetBytes(raw, "error.type").String()
			message := gjson.GetBytes(raw, "error.message").String()
			writeOpenAICompactSSEFailureMessage(c, status, code, message)
			return
		}
		c.Data(status, "application/json", raw)
	}
	fail := func(status int, code, message string) {
		raw, _ := json.Marshal(gin.H{"error": gin.H{"type": code, "message": message}})
		writeError(status, raw)
	}
	fallback := func(code string) (*OpenAIForwardResult, error) {
		ClearPrismBrowserAttempt(c, account.ID)
		ClearActualOpenAIUpstreamEndpoint(c)
		return nil, fmt.Errorf("%w: %s", errPrismBrowserHTTPFallback, code)
	}
	if !accountUsesPrismBrowser(account, s.cfg) {
		return fallback("prism_disabled")
	}
	requestedModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if reason := prismBrowserUnsupportedClientRequest(c, body, requestedModel); reason != "" {
		fail(http.StatusUnprocessableEntity, "unsupported_request", reason)
		return nil, errors.New(reason)
	}
	if prismBrowserRequestHasEncryptedReasoning(body) {
		return fallback("encrypted_reasoning")
	}
	if prismBrowserRequestHasStructuredOutput(body) {
		return fallback("structured_output")
	}
	if requestedModel == "" {
		fail(http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, errors.New("prism adapter model is required")
	}
	if prismBrowserCanonicalModel(account.GetMappedModel(requestedModel)) != "gpt-6.1-sol" && !prismBrowserCompactSpelling(requestedModel) {
		return fallback("unsupported_model")
	}
	prepared, originalModel, upstreamModel, err := prismBrowserPrepareAdapterBody(account, body)
	if err != nil {
		fail(http.StatusBadRequest, "invalid_request_error", "invalid Prism request")
		return nil, err
	}
	sessionID, err := prismBrowserSessionID(c, account.ID, prepared)
	if err != nil {
		fail(http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	stashOpenAIUsageEstimateRequestBody(c, prepared)
	stream := gjson.GetBytes(prepared, "stream").Bool()
	responseBody, upstreamHeaders, status, err := s.callPrismBrowserForCaller(ctx, account, prepared, sessionID, prismBrowserCallerID(c, account.ID))
	if prismBrowserShouldHTTPFallback(err, status, responseBody) {
		return fallback("prism_unavailable")
	}
	if err != nil {
		fail(http.StatusBadGateway, "prism_unavailable", "Prism adapter unavailable")
		return nil, err
	}
	if status != http.StatusOK {
		writeError(status, responseBody)
		return nil, prismBrowserForwardError(status, responseBody)
	}
	responseID, err := prismBrowserTerminal(responseBody, upstreamModel, stream)
	if err != nil {
		fail(http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned no valid terminal response")
		return nil, err
	}
	if err := prismBrowserValidateToolCatalog(prepared, responseBody, stream); err != nil {
		fail(http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned an undeclared client tool")
		return nil, err
	}
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	SetActualOpenAIUpstreamEndpoint(c, prismBrowserUpstreamEndpoint)
	c.Header("X-Prism-Usage", "estimated")
	c.Data(http.StatusOK, contentType, responseBody)
	result := &OpenAIForwardResult{
		RequestID:        responseID,
		ResponseID:       responseID,
		UpstreamHeaders:  upstreamHeaders,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           stream,
		Duration:         time.Since(started),
		UpstreamEndpoint: prismBrowserUpstreamEndpoint,
		ReasoningEffort:  extractOpenAIReasoningEffortFromBody(prepared, upstreamModel, originalModel),
	}
	if requested := CanonicalRequestedReasoningEffort(body, originalModel); requested != nil {
		result.RequestedReasoningEffort = requested
	}
	usage := OpenAIUsage{}
	applyEstimatedOpenAIUsageIfMissing(&usage, upstreamModel, prepared, prismBrowserTerminalOutputText(responseBody, stream))
	result.Usage = usage
	return result, nil
}

func (s *OpenAIGatewayService) callPrismBrowser(ctx context.Context, account *Account, body []byte) ([]byte, http.Header, int, error) {
	return s.callPrismBrowserWithSession(ctx, account, body, "")
}

func (s *OpenAIGatewayService) callPrismBrowserWithSession(ctx context.Context, account *Account, body []byte, sessionID string) ([]byte, http.Header, int, error) {
	return s.callPrismBrowserForCaller(ctx, account, body, sessionID, "")
}

func (s *OpenAIGatewayService) callPrismBrowserForCaller(ctx context.Context, account *Account, body []byte, sessionID, callerID string) ([]byte, http.Header, int, error) {
	if !accountUsesPrismBrowser(account, s.cfg) {
		return nil, nil, 0, errors.New("prism adapter is disabled")
	}
	endpoint, err := prismBrowserAdapterURL(s.cfg.Gateway.PrismBrowser.BaseURL)
	if err != nil {
		return nil, nil, 0, err
	}
	key := strings.TrimSpace(s.cfg.Gateway.PrismBrowser.APIKey)
	if key == "" {
		return nil, nil, 0, errors.New("prism adapter key is not configured")
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, nil, 0, err
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, nil, 0, errors.New("invalid Prism OAuth token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Prism-Account-ID", strconv.FormatInt(account.ID, 10))
	req.Header.Set("X-Prism-OAuth-Token", token)
	if sessionID != "" {
		req.Header.Set("X-Prism-Session-ID", sessionID)
	}
	if callerID != "" {
		req.Header.Set("X-Prism-Caller-ID", callerID)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Timeout:       5 * time.Minute,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("prism adapter request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, prismBrowserMaxResponseBytes+1))
	if err != nil || len(responseBody) > prismBrowserMaxResponseBytes {
		return nil, nil, 0, errors.New("prism adapter response exceeded limit")
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, nil, 0, errors.New("prism adapter redirected unexpectedly")
	}
	return responseBody, resp.Header, resp.StatusCode, nil
}

func prismBrowserCallerID(c *gin.Context, accountID int64) string {
	if c == nil || accountID <= 0 {
		return ""
	}
	keyID := getAPIKeyIDFromContext(c)
	if keyID <= 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("prism-caller-v1:%d:%d", keyID, accountID)))
	return hex.EncodeToString(sum[:])
}

func prismBrowserSessionID(c *gin.Context, accountID int64, body []byte) (string, error) {
	if c == nil || c.Request == nil {
		return "", nil
	}
	for _, names := range [][]string{openAIThreadIdentityHeaders, openAISessionIdentityHeaders} {
		for _, name := range names {
			if len(c.Request.Header.Values(name)) > 1 {
				return "", errors.New("prism conversation identity headers must not be repeated")
			}
		}
	}
	resolution := resolveOpenAIClientSessionIdentity(c, body)
	switch resolution.metadata.Status {
	case openAIClientSessionIdentityMissing:
		return "", nil
	case openAIClientSessionIdentityResolved:
	default:
		return "", errors.New("prism conversation identity is invalid or conflicting")
	}
	keyID := getAPIKeyIDFromContext(c)
	if keyID <= 0 || accountID <= 0 {
		return "", errors.New("prism session reuse requires an authenticated API key")
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("prism-session-v1:%d:%d:%s:%s",
		keyID, accountID, resolution.identity.kind, resolution.identity.value)))
	return hex.EncodeToString(digest[:]), nil
}
