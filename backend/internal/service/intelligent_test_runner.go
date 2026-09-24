package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/gin-gonic/gin"
)

type intelligentRunKey struct{}
type intelligentRunContext struct {
	prompt   string
	testType string
	capture  *intelligentCapture
}

// ChatGPT Codex plan-gates gpt-5.3-codex and gpt-5.4. Empty ChatGPT OAuth
// intelligent tests use gpt-6-astra with explicit low reasoning; omitting
// reasoning.effort lets Codex default to medium.
const (
	intelligentTestDefaultCodexModel      = "gpt-6-astra"
	intelligentTestDefaultReasoningEffort = "low"
	intelligentCaptureRawLimit            = 256 << 10
	intelligentCaptureTextLimit           = 512 << 10
	intelligentCaptureUpstreamLimit       = 64 << 20
)

// Only an empty selection receives the protocol default. Explicit choices
// remain observable even when the upstream rejects the requested model.
func pelicanTestGroupName(account *Account) string {
	if account == nil {
		return ""
	}
	for _, group := range account.Groups {
		if group != nil && VipDiscountedGroup(group.Name) {
			return strings.TrimSpace(group.Name)
		}
	}
	return ""
}

func resolveIntelligentTestModel(account *Account, configured string) string {
	model := strings.TrimSpace(configured)
	if model != "" {
		return model
	}
	if account != nil && account.IsOpenAIOAuthLike() {
		return intelligentTestDefaultCodexModel
	}
	return model
}

func intelligentContext(ctx context.Context) *intelligentRunContext {
	v, _ := ctx.Value(intelligentRunKey{}).(*intelligentRunContext)
	return v
}
func intelligentPrompt(ctx context.Context) string {
	if v := intelligentContext(ctx); v != nil {
		return v.prompt
	}
	return ""
}
func applyIntelligentPayloadPrompt(ctx context.Context, payload map[string]any) {
	prompt := intelligentPrompt(ctx)
	if prompt == "" {
		return
	}
	if _, ok := payload["input"]; ok {
		payload["input"] = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": prompt}}}}
	}
	if _, ok := payload["messages"]; ok {
		payload["messages"] = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": prompt}}}}
		if _, bounded := payload["max_tokens"]; bounded {
			payload["max_tokens"] = 8192
		}
	}
	applyIntelligentTestReasoning(payload)
}

func applyIntelligentTestReasoning(payload map[string]any) {
	if payload == nil {
		return
	}
	if _, ok := payload["input"]; ok {
		payload["reasoning"] = map[string]any{"effort": intelligentTestDefaultReasoningEffort}
		return
	}
	if _, ok := payload["messages"]; ok {
		if _, hasSystem := payload["system"]; hasSystem {
			return
		}
		payload["reasoning_effort"] = intelligentTestDefaultReasoningEffort
	}
}

// applyIntelligentTestOpenAICodexTicket 让 GPT OAuth 智能测试走和生产转发相同的
// 292 门票注入。普通「测试连接」不含 intelligent 上下文，仍发不带门票的探测。
// 鹈鹕测试在本号没有可用票时，借用其他号上带 Cookie 的 292，出站仍同时带上头和 Cookie。
func (s *AccountTestService) applyIntelligentTestOpenAICodexTicket(ctx context.Context, account *Account, body []byte, h http.Header) error {
	if s == nil || s.openaiGatewayService == nil || intelligentContext(ctx) == nil {
		return nil
	}
	model := extractOpenAICodexTicketModel(body)
	err := s.openaiGatewayService.applyOpenAICodexTicket(ctx, account, model, h)
	if s.openaiGatewayService.openAICookieWSModeConfigured() {
		// Cookie mode owns its direct WS adapter. Never borrow legacy material
		// or fall back to an unvalidated HTTP probe for this mode.
		return err
	}
	if openAICodexTicketInjected(h, s.openaiGatewayService.openAICodexTicketConfig().TargetLength) {
		return nil
	}
	if intelligentContext(ctx).testType != "pelican" {
		return err
	}
	exceptID := int64(0)
	if account != nil {
		exceptID = account.ID
	}
	borrowed := s.openaiGatewayService.lookupBorrowedOpenAICodexTicket(ctx, exceptID, model)
	if borrowed.usable(s.openaiGatewayService.openAICodexTicketConfig().TargetLength) {
		h.Set(openAICodexTurnStateHeader, borrowed.State)
		applyOpenAICodexTicketCookies(h, borrowed.Cookies)
		return nil
	}
	return err
}

// RunIntelligentTest uses the existing authenticated outbound protocol adapters.
// A per-run service avoids mutating shared service state. Runtime health writes
// are suppressed: an observation never changes account policy or scheduling.
// ChatGPT OAuth 出站复用网关 applyOpenAICodexTicket：有票则覆盖 x-codex-turn-state，
// 并带上打票时保存的 Cookie。fail-closed 无票则与业务请求一样拒绝，不裸打门控模型。
func (s *AccountTestService) RunIntelligentTest(ctx context.Context, r *IntelligentTestRecord) error {
	account, err := s.accountRepo.GetByID(ctx, r.AccountID)
	if err != nil {
		r.Status = "account_error"
		return errors.New("account unavailable")
	}
	r.AntiDegradation = account.AntiDegradationEnabled()
	if r.ConfigSnapshot == nil {
		return errors.New("missing configuration")
	}
	r.Model = resolveIntelligentTestModel(account, r.ConfigSnapshot.Model)
	if err := validateIntelligentTextModel(account.GetMappedModel(r.Model)); err != nil {
		r.Status = "request_error"
		return err
	}
	r.ConfigSnapshot.Execution = snapshotIntelligentProtectionRuntime(account)
	r.ConfigSnapshot.Execution.RequestedModel = r.ConfigSnapshot.Model
	r.ConfigSnapshot.Execution.Model = r.Model
	r.ConfigSnapshot.Execution.ReasoningEffort = intelligentTestDefaultReasoningEffort
	r.ConfigSnapshot.Execution.GroupName = pelicanTestGroupName(account)
	capture := &intelligentCapture{}
	capture.collectCredentialSecrets(account.Credentials)
	ctx = context.WithValue(ctx, intelligentRunKey{}, &intelligentRunContext{prompt: r.Input, testType: r.TestType, capture: capture})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	recorder := &intelligentSSEWriter{header: http.Header{}, cancel: cancel}
	c, _ := gin.CreateTestContext(recorder)
	c.Request = (&http.Request{Method: http.MethodPost, Header: http.Header{}}).WithContext(runCtx)
	readRepo := &intelligentReadOnlyAccountRepo{AccountRepository: s.accountRepo, capture: capture, snapshot: account}
	clone := NewAccountTestService(readRepo, s.claudeTokenProvider, s.grokTokenProvider, &intelligentHTTPUpstream{inner: s.httpUpstream, capture: capture}, s.cfg, s.tlsFPProfileService)
	clone.settingService = s.settingService
	clone.pluginManager = s.pluginManager
	clone.openaiGatewayService = s.openaiGatewayService
	clone.agentIdentityWS = s.agentIdentityWS
	clone.grokWSDialer = s.grokWSDialer
	prepared, err := readRepo.GetByID(ctx, r.AccountID)
	if err != nil {
		r.Status = "account_error"
		return errors.New("account unavailable")
	}
	if err := prepareIntelligentTestProtection(c, prepared, map[string]any{}); err != nil {
		r.Status = "request_error"
		return err
	}
	if prepared.IsCNProvider() && prepared.GetAPIProtocol() == APIProtocolAdaptive {
		err = clone.testCNProviderChatCompletionsConnection(c, prepared, r.Model, r.Input)
	} else {
		err = clone.TestAccountConnection(c, prepared.ID, r.Model, r.Input, AccountTestModeDefault)
	}
	textOutput, eventError, model, _ := parseIntelligentSSE(recorder.body.String())
	if capture.trafficWait != nil && capture.status == 0 {
		return &TestAdmissionWaitError{Until: time.Now().Add(capture.trafficWait.RetryAfter), Reason: capture.trafficWait.Reason}
	}
	return finalizeIntelligentTestRun(r, capture, recorder, textOutput, eventError, model, err)
}

func finalizeIntelligentTestRun(r *IntelligentTestRecord, capture *intelligentCapture, recorder *intelligentSSEWriter, textOutput, eventError, model string, err error) error {
	complete := false
	textOverflow := false
	if recorder != nil {
		recorder.flush()
		if t := recorder.outputText(); t != "" {
			textOutput = t
		}
		if recorder.errMsg != "" {
			eventError = recorder.errMsg
		}
		if recorder.model != "" {
			model = recorder.model
		}
		complete = recorder.sawComplete
		textOverflow = recorder.truncated
	}
	if capture != nil {
		if t := capture.outputText(); t != "" || capture.authoritativeText {
			textOutput = t
		}
		if capture.responsesStream() {
			complete = capture.upstreamComplete()
		}
		textOverflow = textOverflow || capture.textTruncated
	}
	if model != "" {
		r.Model = model
	}
	if r != nil && r.ConfigSnapshot != nil && r.ConfigSnapshot.Execution != nil {
		r.ConfigSnapshot.Execution.Model = r.Model
	}
	redact := func(s string) string { return s }
	if capture != nil {
		redact = capture.redact
	}
	if r == nil {
		return errors.New("missing test record")
	}
	r.Result = redact(textOutput)
	if capture != nil {
		r.RawResponse = redact(capture.body.String())
		r.RawTruncated = capture.truncated
	}
	if recorder != nil {
		r.RawTruncated = r.RawTruncated || recorder.truncated
		if r.RawResponse == "" {
			r.RawResponse = redact(recorder.body.String())
		}
	}
	r.ErrorMessage = redact(eventError)
	if r.ErrorMessage == "" && err != nil {
		r.ErrorMessage = redact(err.Error())
	}
	if len(r.Result) > intelligentCaptureTextLimit {
		r.Result = r.Result[:intelligentCaptureTextLimit]
		r.RawTruncated = true
		textOverflow = true
		err = errors.New("test output exceeded capture limit")
	}
	if len(r.ErrorMessage) > 4096 {
		r.ErrorMessage = r.ErrorMessage[:4096]
	}
	r.Result = strings.ToValidUTF8(r.Result, "�")
	r.RawResponse = strings.ToValidUTF8(r.RawResponse, "�")
	r.ErrorMessage = strings.ToValidUTF8(r.ErrorMessage, "�")
	if err != nil || eventError != "" || !complete || strings.TrimSpace(r.Result) == "" || textOverflow {
		if err == nil {
			err = errors.New("upstream did not return a complete, nonempty test result")
		}
		if r.ErrorMessage == "" {
			r.ErrorMessage = err.Error()
		}
		status := 0
		if capture != nil {
			status = capture.status
		}
		r.Status = classifyIntelligentError(status, r.ErrorMessage)
		var cookieErr *openAICookieWSTestError
		if errors.As(err, &cookieErr) {
			r.Status = cookieErr.Category
		}
		return err
	}
	return nil
}

// Some old connectivity adapters treat EOF as success. Capability scoring
// requires a real terminal event and rejects token-budget truncation.
func intelligentRawComplete(raw string) bool {
	complete, truncated := false, false
	var inspect func(map[string]any)
	inspect = func(v map[string]any) {
		if nested, ok := v["response"].(map[string]any); ok {
			inspect(nested)
		}
		kind, _ := v["type"].(string)
		if kind == "response.completed" || kind == "response.done" || kind == "message_stop" {
			complete = true
		}
		if status, _ := v["status"].(string); status == "incomplete" || status == "failed" {
			truncated = true
		}
		if reason, _ := v["stop_reason"].(string); reason != "" {
			if reason == "max_tokens" {
				truncated = true
			} else {
				complete = true
			}
		}
		if delta, ok := v["delta"].(map[string]any); ok {
			inspect(delta)
		}
		if choices, ok := v["choices"].([]any); ok {
			for _, item := range choices {
				if choice, ok := item.(map[string]any); ok {
					if reason, _ := choice["finish_reason"].(string); reason != "" {
						if reason == "length" || reason == "content_filter" {
							truncated = true
						} else {
							complete = true
						}
					}
				}
			}
		}
		if candidates, ok := v["candidates"].([]any); ok {
			for _, item := range candidates {
				if candidate, ok := item.(map[string]any); ok {
					if reason, _ := candidate["finishReason"].(string); reason != "" {
						if reason == "STOP" {
							complete = true
						} else {
							truncated = true
						}
					}
				}
			}
		}
	}
	var body map[string]any
	if json.Unmarshal([]byte(raw), &body) == nil {
		inspect(body)
	} else {
		scanner := bufio.NewScanner(strings.NewReader(raw))
		scanner.Buffer(make([]byte, 8192), 4<<20)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) == nil {
				inspect(event)
			}
		}
	}
	return complete && !truncated
}
func parseIntelligentSSE(raw string) (output, errorMessage, model string, complete bool) {
	var text strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 8192), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var e TestEvent
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &e) != nil {
			continue
		}
		switch e.Type {
		case "content":
			text.WriteString(e.Text)
		case "error":
			errorMessage = e.Error
		case "test_start":
			model = e.Model
		case "test_complete":
			complete = e.Success
		}
	}
	return text.String(), errorMessage, model, complete
}
func classifyIntelligentError(code int, message string) string {
	lower := strings.ToLower(message)
	switch {
	case code == 429 || strings.Contains(lower, "rate limit") || strings.Contains(lower, "限流"):
		return "rate_limited"
	case code == 401 || code == 403:
		return "account_error"
	case code >= 500:
		return "network_error"
	case strings.Contains(lower, "max_tokens") || strings.Contains(lower, "max_output_tokens") || strings.Contains(lower, "max_completion_tokens"):
		return "request_error"
	case isOpenAICodexPlanGatedModelError(code, []byte(message)) || strings.Contains(lower, "model is not supported when using codex"):
		return "model_error"
	case code == 404 || strings.Contains(lower, "model not found") || strings.Contains(lower, "unsupported model"):
		return "model_error"
	case code == 400 || code == 422:
		return "request_error"
	case strings.Contains(lower, "codex turn-state ticket unavailable"):
		return "account_error"
	case strings.Contains(lower, "expired token") || strings.Contains(lower, "access token") || strings.Contains(lower, "credential") || strings.Contains(lower, "api key") || strings.Contains(lower, "account not found") || strings.Contains(message, "账号认证"):
		return "account_error"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "connection") || strings.Contains(lower, "deadline"):
		return "network_error"
	default:
		return "failed"
	}
}

func validateIntelligentTextModel(model string) error {
	lower := strings.ToLower(strings.TrimSpace(model))
	if isOpenAIImageModel(model) || strings.HasPrefix(lower, "grok-imagine") || strings.HasPrefix(lower, "tts-") || strings.HasPrefix(lower, "whisper") || strings.HasPrefix(lower, "sora") {
		return errors.New("当前题目需要文本生成模型；鹈鹕测试通过 SVG 源码绘图，请勿选择原生图片、音频或视频模型")
	}
	return nil
}

type intelligentSSEWriter struct {
	header      http.Header
	body        bytes.Buffer
	pending     []byte
	text        strings.Builder
	model       string
	errMsg      string
	cancel      context.CancelFunc
	truncated   bool
	sawComplete bool
}

func (w *intelligentSSEWriter) Header() http.Header { return w.header }
func (w *intelligentSSEWriter) WriteHeader(int)     {}
func (w *intelligentSSEWriter) Flush()              {}
func (w *intelligentSSEWriter) outputText() string {
	if w == nil {
		return ""
	}
	w.flush()
	return w.text.String()
}
func (w *intelligentSSEWriter) flush() {
	if w == nil || len(w.pending) == 0 {
		return
	}
	line := bytes.TrimSpace(w.pending)
	w.pending = nil
	if len(line) > 0 {
		w.ingestTestEventLine(line)
	}
}
func (w *intelligentSSEWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(w.pending[:i])
		w.pending = w.pending[i+1:]
		if len(line) == 0 {
			continue
		}
		w.ingestTestEventLine(line)
		if w.body.Len() < intelligentCaptureRawLimit {
			remain := intelligentCaptureRawLimit - w.body.Len()
			chunk := append(append([]byte(nil), line...), '\n')
			if len(chunk) > remain {
				_, _ = w.body.Write(chunk[:remain])
			} else {
				_, _ = w.body.Write(chunk)
			}
		}
	}
	if w.text.Len() > intelligentCaptureTextLimit {
		w.truncated = true
		if w.cancel != nil {
			w.cancel()
		}
		return 0, errors.New("test output too large")
	}
	return len(p), nil
}
func (w *intelligentSSEWriter) ingestTestEventLine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	var e TestEvent
	if json.Unmarshal(payload, &e) != nil {
		return
	}
	switch e.Type {
	case "content":
		w.appendText(e.Text)
	case "error":
		w.errMsg = e.Error
	case "test_start":
		w.model = e.Model
	case "test_complete":
		w.sawComplete = e.Success
	}
}
func (w *intelligentSSEWriter) appendText(s string) {
	if s == "" || w.truncated {
		return
	}
	if w.text.Len()+len(s) > intelligentCaptureTextLimit {
		remain := intelligentCaptureTextLimit - w.text.Len()
		if remain > 0 {
			w.text.WriteString(s[:remain])
		}
		w.truncated = true
		return
	}
	w.text.WriteString(s)
}

type intelligentCapture struct {
	trafficWait       *AccountTrafficLimitError
	mu                sync.Mutex
	body              bytes.Buffer
	pending           []byte
	text              strings.Builder
	status            int
	truncated         bool
	textTruncated     bool
	complete          bool
	incomplete        bool
	sawOutputDelta    bool
	authoritativeText bool
	secrets           []string
}

func (c *intelligentCapture) outputText() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushPendingLocked()
	return c.text.String()
}
func (c *intelligentCapture) upstreamComplete() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushPendingLocked()
	return c.complete && !c.incomplete && !c.textTruncated
}
func (c *intelligentCapture) responsesStream() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushPendingLocked()
	return c.sawOutputDelta || c.complete || c.incomplete
}
func (c *intelligentCapture) flushPendingLocked() {
	if len(c.pending) == 0 {
		return
	}
	line := bytes.TrimSpace(c.pending)
	c.pending = nil
	if len(line) > 0 {
		c.ingestUpstreamLine(line)
	}
}
func (c *intelligentCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if c.body.Len() < intelligentCaptureRawLimit {
		remain := intelligentCaptureRawLimit - c.body.Len()
		if remain < len(p) {
			_, _ = c.body.Write(p[:remain])
			c.truncated = true
		} else {
			_, _ = c.body.Write(p)
		}
	} else {
		c.truncated = true
	}
	c.pending = append(c.pending, p...)
	for {
		i := bytes.IndexByte(c.pending, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(c.pending[:i])
		c.pending = c.pending[i+1:]
		if len(line) == 0 {
			continue
		}
		c.ingestUpstreamLine(line)
	}
	return n, nil
}
func (c *intelligentCapture) ingestUpstreamLine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	c.ingestUpstreamEvent(event)
}
func (c *intelligentCapture) ingestUpstreamEvent(v map[string]any) {
	kind, _ := v["type"].(string)
	switch kind {
	case "response.output_text.delta":
		c.sawOutputDelta = true
		if delta, ok := v["delta"].(string); ok && !c.complete {
			c.appendText(delta)
		}
	case "response.completed", "response.done":
		if !intelligentResponsesTerminalSuccessful(v) {
			c.incomplete = true
			c.retainFailedResponseText(v)
			break
		}
		if text, present := intelligentResponsesTerminalText(v); present {
			// The completed response is the authoritative ordered output. It
			// repairs missing/interleaved deltas without appending duplicates.
			c.text.Reset()
			c.textTruncated = false
			c.authoritativeText = true
			c.appendText(text)
		}
		c.complete = true
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		c.incomplete = true
		c.retainFailedResponseText(v)
	case "message_stop":
		c.complete = true
	}
	if nested, ok := v["response"].(map[string]any); ok {
		c.ingestUpstreamEvent(nested)
	}
	if status, _ := v["status"].(string); status == "incomplete" || status == "failed" || status == "cancelled" || status == "canceled" {
		c.incomplete = true
	}
	if reason, _ := v["stop_reason"].(string); reason != "" {
		if reason == "max_tokens" {
			c.incomplete = true
		} else {
			c.complete = true
		}
	}
	if delta, ok := v["delta"].(map[string]any); ok {
		if text, ok := delta["text"].(string); ok {
			c.appendText(text)
		}
		if text, ok := delta["content"].(string); ok {
			c.appendText(text)
		}
	}
	if choices, ok := v["choices"].([]any); ok {
		for _, item := range choices {
			choice, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if reason, _ := choice["finish_reason"].(string); reason != "" {
				if reason == "length" || reason == "content_filter" {
					c.incomplete = true
				} else {
					c.complete = true
				}
			}
		}
	}
	if candidates, ok := v["candidates"].([]any); ok {
		for _, item := range candidates {
			candidate, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if reason, _ := candidate["finishReason"].(string); reason != "" {
				if reason == "STOP" {
					c.complete = true
				} else {
					c.incomplete = true
				}
			}
		}
	}
}

func (c *intelligentCapture) retainFailedResponseText(event map[string]any) {
	if text, present := intelligentResponsesTerminalText(event); present && text != "" {
		// Failure output is diagnostic only; this never clears incomplete.
		c.text.Reset()
		c.textTruncated = false
		c.appendText(text)
	}
}

func intelligentResponsesTerminalSuccessful(event map[string]any) bool {
	kind, _ := event["type"].(string)
	if kind != "response.completed" && kind != "response.done" {
		return false
	}
	if event["error"] != nil {
		return false
	}
	if status, _ := event["status"].(string); status != "" && status != "completed" {
		return false
	}
	response, _ := event["response"].(map[string]any)
	if response["error"] != nil {
		return false
	}
	if status, _ := response["status"].(string); status != "" && status != "completed" {
		return false
	}
	if output, ok := response["output"].([]any); ok {
		for _, value := range output {
			item, _ := value.(map[string]any)
			if status, _ := item["status"].(string); status != "" && status != "completed" {
				return false
			}
		}
	}
	return true
}

func intelligentResponsesTerminalText(event map[string]any) (string, bool) {
	response, _ := event["response"].(map[string]any)
	output, present := response["output"].([]any)
	if !present {
		return "", false
	}
	var text strings.Builder
	for _, value := range output {
		item, _ := value.(map[string]any)
		content, _ := item["content"].([]any)
		for _, value := range content {
			part, _ := value.(map[string]any)
			if part["type"] != "output_text" {
				continue
			}
			partText, _ := part["text"].(string)
			remaining := intelligentCaptureTextLimit + 1 - text.Len()
			if len(partText) > remaining {
				partText = partText[:remaining]
			}
			text.WriteString(partText)
			if text.Len() > intelligentCaptureTextLimit {
				return text.String(), true
			}
		}
	}
	return text.String(), true
}

func (c *intelligentCapture) appendText(s string) {
	if s == "" || c.textTruncated {
		return
	}
	if c.text.Len()+len(s) > intelligentCaptureTextLimit {
		remain := intelligentCaptureTextLimit - c.text.Len()
		if remain > 0 {
			c.text.WriteString(s[:remain])
		}
		c.textTruncated = true
		return
	}
	c.text.WriteString(s)
}
func (c *intelligentCapture) collectCredentialSecrets(values map[string]any) {
	for key, value := range values {
		if nested, ok := value.(map[string]any); ok {
			c.collectCredentialSecrets(nested)
		}
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "key") || strings.Contains(lower, "cookie") {
			if text, ok := value.(string); ok && len(text) >= 6 {
				c.secrets = append(c.secrets, text)
			}
		}
	}
}
func (c *intelligentCapture) redact(raw string) string {
	for _, secret := range c.secrets {
		raw = strings.ReplaceAll(raw, secret, "[REDACTED]")
	}
	return logredact.RedactText(raw)
}

type intelligentCaptureBody struct {
	io.Reader
	closer io.Closer
}

func (b *intelligentCaptureBody) Close() error { return b.closer.Close() }
func (c *intelligentCapture) response(req *http.Request, resp *http.Response) *http.Response {
	if resp == nil {
		return resp
	}
	c.status = resp.StatusCode
	for _, key := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Cookie"} {
		v := req.Header.Get(key)
		if len(v) > 6 {
			c.secrets = append(c.secrets, v)
			if strings.HasPrefix(v, "Bearer ") {
				c.secrets = append(c.secrets, strings.TrimPrefix(v, "Bearer "))
			}
		}
	}
	if resp.Body != nil {
		if _, ok := resp.Body.(*intelligentCaptureBody); !ok {
			resp.Body = &intelligentCaptureBody{Reader: io.TeeReader(io.LimitReader(resp.Body, intelligentCaptureUpstreamLimit), c), closer: resp.Body}
		}
	}
	return resp
}

type intelligentHTTPUpstream struct {
	inner   HTTPUpstream
	capture *intelligentCapture
}

func (h *intelligentHTTPUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	resp, err := h.inner.Do(req, proxy, id, concurrency)
	var wait *AccountTrafficLimitError
	if errors.As(err, &wait) {
		h.capture.trafficWait = wait
	}
	return h.capture.response(req, resp), err
}
func (h *intelligentHTTPUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, p *tlsfingerprint.Profile) (*http.Response, error) {
	resp, err := h.inner.DoWithTLS(req, proxy, id, concurrency, p)
	var wait *AccountTrafficLimitError
	if errors.As(err, &wait) {
		h.capture.trafficWait = wait
	}
	return h.capture.response(req, resp), err
}
func (h *intelligentHTTPUpstream) AccountTrafficController() *AccountTrafficService {
	return accountTrafficController(h.inner)
}
func captureIntelligentResponse(req *http.Request, resp *http.Response) *http.Response {
	if v := intelligentContext(req.Context()); v != nil {
		return v.capture.response(req, resp)
	}
	return resp
}

type intelligentReadOnlyAccountRepo struct {
	AccountRepository
	capture  *intelligentCapture
	snapshot *Account
}

func (r *intelligentReadOnlyAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	a := r.snapshot
	if a == nil || a.ID != id {
		var err error
		a, err = r.AccountRepository.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	copy := *a
	raw, _ := json.Marshal(a.Extra)
	copy.Extra = nil
	_ = json.Unmarshal(raw, &copy.Extra)
	raw, _ = json.Marshal(a.Credentials)
	copy.Credentials = nil
	_ = json.Unmarshal(raw, &copy.Credentials)
	r.capture.collectCredentialSecrets(copy.Credentials)
	return &copy, nil
}
func (r *intelligentReadOnlyAccountRepo) SetError(context.Context, int64, string) error { return nil }
func (r *intelligentReadOnlyAccountRepo) ClearError(context.Context, int64) error       { return nil }
func (r *intelligentReadOnlyAccountRepo) SetRateLimited(context.Context, int64, time.Time) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) ClearRateLimit(context.Context, int64) error { return nil }
func (r *intelligentReadOnlyAccountRepo) SetModelRateLimit(context.Context, int64, string, time.Time, ...string) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) SetOverloaded(context.Context, int64, time.Time) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) SetSchedulable(context.Context, int64, bool) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) Update(context.Context, *Account) error {
	return errors.New("capability tests do not modify account settings; refresh account authentication separately")
}
func (r *intelligentReadOnlyAccountRepo) Create(context.Context, *Account) error {
	return errors.New("capability tests cannot create accounts")
}
func (r *intelligentReadOnlyAccountRepo) Delete(context.Context, int64) error {
	return errors.New("capability tests cannot delete accounts")
}
func (r *intelligentReadOnlyAccountRepo) UpdateLastUsed(context.Context, int64) error { return nil }
func (r *intelligentReadOnlyAccountRepo) BatchUpdateLastUsed(context.Context, map[int64]time.Time) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) AutoPauseExpiredAccounts(context.Context, time.Time) (int64, error) {
	return 0, nil
}
func (r *intelligentReadOnlyAccountRepo) BindGroups(context.Context, int64, []int64) error {
	return errors.New("capability tests cannot change account groups")
}
func (r *intelligentReadOnlyAccountRepo) ClearTempUnschedulable(context.Context, int64) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) ClearModelRateLimits(context.Context, int64) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) UpdateSessionWindow(context.Context, int64, *time.Time, *time.Time, string) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) UpdateSessionWindowEnd(context.Context, int64, time.Time) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) BulkUpdate(context.Context, []int64, AccountBulkUpdate) (int64, error) {
	return 0, errors.New("capability tests cannot bulk edit account settings")
}
func (r *intelligentReadOnlyAccountRepo) IncrementQuotaUsed(context.Context, int64, float64) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) ResetQuotaUsedAndClearRateLimitCooldown(context.Context, int64) error {
	return nil
}
func (r *intelligentReadOnlyAccountRepo) RevertProxyFallback(context.Context, int64) error {
	return errors.New("capability tests cannot change account proxy settings")
}

var _ HTTPUpstream = (*intelligentHTTPUpstream)(nil)
