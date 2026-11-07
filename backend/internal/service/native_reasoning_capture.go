package service

// Request-local capture adapters. They never change, retry, log, or independently
// read/write traffic. A failed observation only makes the state uncacheable.

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

const nativeReasoningCaptureMaxFrames = 8192

// Wire framing can be larger than the logical message (added/done/terminal and
// deltas repeat content). Both limits are hard: a huge stream of tiny deltas or
// comments cannot evade the logical entry bound.
func nativeReasoningCaptureWireLimit(limits NativeReasoningReplayLimits) int {
	return min(limits.MaxHistoryBytes, 8*limits.MaxEntryBytes)
}

type nativeReasoningSSECapture struct {
	maxFrame, maxWire                   int
	wire, frames                        int
	line, data                          []byte
	eventType                           string
	hasData, skipLF, firstLine, invalid bool
	halted                              bool
}

func newNativeReasoningSSECapture(limits NativeReasoningReplayLimits) nativeReasoningSSECapture {
	return nativeReasoningSSECapture{maxFrame: limits.MaxEntryBytes, maxWire: nativeReasoningCaptureWireLimit(limits), firstLine: true}
}
func (p *nativeReasoningSSECapture) clear() {
	p.line = nil
	p.data = nil
	p.eventType = ""
	p.hasData = false
}
func (p *nativeReasoningSSECapture) fail() { p.invalid = true; p.clear() }
func (p *nativeReasoningSSECapture) partial() bool {
	return len(p.line) > 0 || len(p.data) > 0 || p.hasData || p.eventType != ""
}
func (p *nativeReasoningSSECapture) feed(raw []byte, event func([]byte, string) bool) bool {
	if p.invalid {
		return false
	}
	if p.halted {
		return true
	}
	for _, character := range raw {
		if p.wire >= p.maxWire {
			p.fail()
			return false
		}
		p.wire++
		if p.skipLF {
			p.skipLF = false
			if character == '\n' {
				continue
			}
		}
		if character == '\r' || character == '\n' {
			line := p.line
			if p.firstLine {
				line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
				p.firstLine = false
			}
			if !p.addLine(line, event) {
				p.fail()
				return false
			}
			if p.halted {
				p.clear()
				return true
			}
			p.line = p.line[:0]
			if character == '\r' {
				p.skipLF = true
			}
			continue
		}
		if len(p.line) >= p.maxFrame {
			p.fail()
			return false
		}
		p.line = append(p.line, character)
	}
	return true
}
func (p *nativeReasoningSSECapture) addLine(line []byte, event func([]byte, string) bool) bool {
	if len(line) == 0 {
		if p.hasData {
			if p.frames >= nativeReasoningCaptureMaxFrames {
				return false
			}
			p.frames++
			data := bytes.TrimSuffix(p.data, []byte{'\n'})
			if !event(data, p.eventType) {
				return false
			}
		}
		p.data = p.data[:0]
		p.eventType = ""
		p.hasData = false
		return true
	}
	if line[0] == ':' {
		return true
	}
	field, value, _ := bytes.Cut(line, []byte{':'})
	value = bytes.TrimPrefix(value, []byte{' '})
	switch string(field) {
	case "event":
		p.eventType = string(value)
	case "data":
		if len(value)+1 > p.maxFrame-len(p.data) {
			return false
		}
		p.data = append(p.data, value...)
		p.data = append(p.data, '\n')
		p.hasData = true
	}
	return true
}

// NativeReasoningResponseCapture observes only bytes read by the real upstream
// consumer. A complete terminal can be snapshotted before EOF, because the
// production Chat converter deliberately stops at that terminal frame.
type NativeReasoningResponseCapture struct {
	body                                   io.ReadCloser
	readMu                                 sync.Mutex
	mu                                     sync.Mutex
	parser                                 nativeReasoningSSECapture
	collector                              *NativeReasoningReplayCollector
	closed, eof, invalid, invalidEncrypted bool
	completed                              json.RawMessage
}

func NewNativeReasoningResponseCapture(body io.ReadCloser, limits NativeReasoningReplayLimits) *NativeReasoningResponseCapture {
	limits = limits.Normalize()
	return &NativeReasoningResponseCapture{body: body, parser: newNativeReasoningSSECapture(limits), collector: NewNativeReasoningReplayCollector(limits), invalid: body == nil}
}
func (c *NativeReasoningResponseCapture) failLocked() {
	c.invalid = true
	c.parser.fail()
	c.collector = nil
	c.completed = nil
}
func (c *NativeReasoningResponseCapture) observe(raw []byte, eventType string) bool {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
		return true
	}
	if !utf8.Valid(raw) {
		return false
	}
	canonical, err := nativeReplayCanonicalObject(raw)
	if err != nil {
		return false
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Response struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(canonical, &envelope) != nil {
		return false
	}
	if envelope.Error.Code == "invalid_encrypted_content" || envelope.Response.Error.Code == "invalid_encrypted_content" {
		c.invalidEncrypted = true
	}
	// The canonical copy validates framing and error classification only. The
	// collector also needs original done-item RawMessages for sparse terminals.
	c.collector.Observe(raw, eventType)
	if completed, ok := c.collector.CompletedResponse(); ok {
		// Both the production Chat consumer and fixed Codex responses SSE
		// consumer return at the first complete terminal event. Fixed Codex
		// bfdbf01: codex-api/src/sse/responses.rs:667-675 returns after
		// Completed; core/session/turn.rs:3348-3402 then breaks the turn loop.
		// Bytes read ahead after that boundary are not part of this response.
		// The caller still needs handler success, an uncancelled context and
		// the independent client's finish+[DONE] delivery evidence to commit.
		c.completed = completed
		c.collector = nil
		c.parser.halted = true
	}
	return true
}
func (c *NativeReasoningResponseCapture) Read(p []byte) (int, error) {
	if c.body == nil {
		return 0, io.EOF
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	n, err := c.body.Read(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.invalid || len(c.completed) > 0 {
		return n, err
	}
	if n < 0 || n > len(p) {
		c.failLocked()
		return n, err
	}
	if c.eof && n > 0 {
		c.failLocked()
		return n, err
	}
	if !c.parser.feed(p[:n], c.observe) {
		c.failLocked()
	}
	if len(c.completed) > 0 {
		return n, err
	}
	if err != nil {
		if err != io.EOF || c.parser.partial() {
			c.failLocked()
		} else {
			c.eof = true
		}
	}
	return n, err
}

func (c *NativeReasoningResponseCapture) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		if len(c.completed) == 0 {
			c.failLocked()
		} else {
			c.parser.clear()
			c.collector = nil
		}
	}
	c.mu.Unlock()
	if c.body == nil {
		return nil
	}
	return c.body.Close()
}
func (c *NativeReasoningResponseCapture) CompletedResponse() (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.invalid || len(c.completed) == 0 {
		return nil, false
	}
	return append(json.RawMessage(nil), c.completed...), true
}
func (c *NativeReasoningResponseCapture) InvalidEncryptedContent() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.invalidEncrypted
}

type nativeReasoningCapturedTool struct {
	id, kind        string
	name, arguments strings.Builder
}

type nativeReasoningCapturedChat struct {
	id, model, finishReason                     string
	role, finished, done                        bool
	logicalBytes                                int
	content, reasoning, reasoningAlias, refusal strings.Builder
	tools                                       map[int]*nativeReasoningCapturedTool
	callIDs                                     map[string]int
}

// NativeReasoningChatCapture records only successful underlying writes. Short
// writes and errors poison the capture even if a partial prefix looks complete.
// The caller still owns delivery, Flush, cancellation and the forwarding result.
type NativeReasoningChatCapture struct {
	gin.ResponseWriter
	writeMu sync.Mutex
	mu      sync.Mutex
	limits  NativeReasoningReplayLimits
	stream  bool
	invalid bool
	parser  nativeReasoningSSECapture
	buffer  []byte
	chat    nativeReasoningCapturedChat
}

func NewNativeReasoningChatCapture(writer gin.ResponseWriter, stream bool, limits NativeReasoningReplayLimits) *NativeReasoningChatCapture {
	limits = limits.Normalize()
	return &NativeReasoningChatCapture{ResponseWriter: writer, stream: stream, limits: limits, invalid: writer == nil, parser: newNativeReasoningSSECapture(limits), chat: nativeReasoningCapturedChat{tools: map[int]*nativeReasoningCapturedTool{}, callIDs: map[string]int{}}}
}
func (c *NativeReasoningChatCapture) failLocked() {
	c.invalid = true
	c.buffer = nil
	c.parser.fail()
	c.chat = nativeReasoningCapturedChat{}
}
func (c *NativeReasoningChatCapture) record(raw []byte) {
	if c.invalid {
		return
	}
	if c.stream {
		if !c.parser.feed(raw, c.observe) {
			c.failLocked()
		}
		return
	}
	if len(raw) > c.limits.MaxEntryBytes-len(c.buffer) || len(raw) > c.parser.maxWire-c.parser.wire {
		c.failLocked()
		return
	}
	c.parser.wire += len(raw)
	c.buffer = append(c.buffer, raw...)
}
func (c *NativeReasoningChatCapture) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ResponseWriter == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := c.ResponseWriter.Write(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || n != len(p) {
		c.failLocked()
	} else {
		c.record(p[:n])
	}
	return n, err
}
func (c *NativeReasoningChatCapture) WriteString(p string) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ResponseWriter == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := c.ResponseWriter.WriteString(p)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil || n != len(p) {
		c.failLocked()
	} else if !c.invalid {
		if len(p) > c.parser.maxWire-c.parser.wire {
			c.failLocked()
		} else {
			c.record([]byte(p[:n]))
		}
	}
	return n, err
}
func nativeReasoningCompleteChatFinish(reason string) bool {
	return reason == "stop" || reason == "tool_calls"
}
func nativeReasoningCaptureObject(raw []byte) (map[string]json.RawMessage, bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	canonical, err := nativeReplayCanonicalObject(raw)
	if err != nil {
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(canonical, &object) != nil {
		return nil, false
	}
	return object, true
}
func (c *NativeReasoningChatCapture) addText(builder *strings.Builder, raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return true
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	if c.chat.finished && value != "" {
		return false
	}
	if len(value) > c.limits.MaxEntryBytes-c.chat.logicalBytes {
		return false
	}
	c.chat.logicalBytes += len(value)
	builder.WriteString(value)
	return true
}
func (c *NativeReasoningChatCapture) observe(raw []byte, eventType string) bool {
	if eventType != "" && eventType != "message" {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
		if !c.chat.finished {
			return false
		}
		c.chat.done = true
		return true
	}
	if c.chat.done || (eventType != "" && eventType != "message") {
		return false
	}
	object, ok := nativeReasoningCaptureObject(raw)
	if !ok {
		return false
	}
	if _, hasError := object["error"]; hasError {
		return false
	}
	var id, kind, model string
	if json.Unmarshal(object["id"], &id) != nil || id == "" || json.Unmarshal(object["object"], &kind) != nil || kind != "chat.completion.chunk" {
		return false
	}
	if len(object["model"]) > 0 && json.Unmarshal(object["model"], &model) != nil {
		return false
	}
	if c.chat.id != "" && c.chat.id != id {
		return false
	}
	c.chat.id = id
	if c.chat.model != "" && model != "" && c.chat.model != model {
		return false
	}
	if model != "" {
		c.chat.model = model
	}
	var choices []struct {
		Index        *int            `json:"index"`
		Delta        json.RawMessage `json:"delta"`
		FinishReason *string         `json:"finish_reason"`
	}
	if json.Unmarshal(object["choices"], &choices) != nil || len(choices) > 1 {
		return false
	}
	if len(choices) == 0 {
		return len(object["usage"]) > 0 && c.chat.role
	}
	choice := choices[0]
	if choice.Index == nil || *choice.Index != 0 {
		return false
	}
	delta, ok := nativeReasoningCaptureObject(choice.Delta)
	if !ok {
		return false
	}
	for key := range delta {
		switch key {
		case "role", "content", "reasoning_content", "reasoning", "refusal", "tool_calls":
		default:
			return false
		}
	}
	if role, exists := delta["role"]; exists {
		var value string
		if json.Unmarshal(role, &value) != nil || value != "assistant" {
			return false
		}
		c.chat.role = true
	}
	if !c.addText(&c.chat.content, delta["content"]) || !c.addText(&c.chat.reasoning, delta["reasoning_content"]) || !c.addText(&c.chat.reasoningAlias, delta["reasoning"]) || !c.addText(&c.chat.refusal, delta["refusal"]) {
		return false
	}
	if tools, exists := delta["tool_calls"]; exists && !bytes.Equal(tools, []byte("null")) {
		if c.chat.finished {
			return false
		}
		var calls []map[string]json.RawMessage
		if json.Unmarshal(tools, &calls) != nil || len(calls) > c.limits.MaxOutputItems {
			return false
		}
		for _, call := range calls {
			for key := range call {
				switch key {
				case "index", "id", "type", "function":
				default:
					return false
				}
			}
			var position *int
			if json.Unmarshal(call["index"], &position) != nil || position == nil || *position < 0 || *position >= c.limits.MaxOutputItems {
				return false
			}
			index := *position
			tool := c.chat.tools[index]
			if tool == nil {
				tool = &nativeReasoningCapturedTool{}
				c.chat.tools[index] = tool
			}
			for _, field := range []struct {
				key    string
				target *string
			}{{"id", &tool.id}, {"type", &tool.kind}} {
				if raw, exists := call[field.key]; exists {
					var value string
					if json.Unmarshal(raw, &value) != nil {
						return false
					}
					if value == "" {
						continue
					}
					if *field.target != "" && *field.target != value {
						return false
					}
					if *field.target == "" {
						if len(value) > c.limits.MaxEntryBytes-c.chat.logicalBytes {
							return false
						}
						c.chat.logicalBytes += len(value)
						*field.target = value
					}
				}
			}
			if tool.kind != "" && tool.kind != "function" {
				return false
			}
			if tool.id != "" {
				if old, exists := c.chat.callIDs[tool.id]; exists && old != index {
					return false
				}
				c.chat.callIDs[tool.id] = index
			}
			if fn, exists := call["function"]; exists {
				function, ok := nativeReasoningCaptureObject(fn)
				if !ok {
					return false
				}
				for key := range function {
					if key != "name" && key != "arguments" {
						return false
					}
				}
				if !c.addText(&tool.name, function["name"]) || !c.addText(&tool.arguments, function["arguments"]) {
					return false
				}
			}
		}
	}
	if choice.FinishReason != nil {
		if !nativeReasoningCompleteChatFinish(*choice.FinishReason) || (c.chat.finished && c.chat.finishReason != *choice.FinishReason) {
			return false
		}
		c.chat.finished = true
		c.chat.finishReason = *choice.FinishReason
	}
	return true
}
func (c *NativeReasoningChatCapture) messageLocked() (json.RawMessage, bool) {
	if c.invalid || c.parser.invalid {
		return nil, false
	}
	if !c.stream {
		return c.bufferedMessageLocked()
	}
	if c.parser.partial() || !c.chat.role || !c.chat.finished || !c.chat.done {
		return nil, false
	}
	message := apicompat.ChatMessage{Role: "assistant", ReasoningContent: c.chat.reasoning.String(), Reasoning: c.chat.reasoningAlias.String(), Refusal: c.chat.refusal.String()}
	if c.chat.content.Len() > 0 {
		message.Content, _ = json.Marshal(c.chat.content.String())
	}
	for index := 0; index < len(c.chat.tools); index++ {
		tool := c.chat.tools[index]
		if tool == nil || tool.id == "" || tool.kind != "function" || tool.name.Len() == 0 {
			return nil, false
		}
		message.ToolCalls = append(message.ToolCalls, apicompat.ChatToolCall{ID: tool.id, Type: tool.kind, Function: apicompat.ChatFunctionCall{Name: tool.name.String(), Arguments: tool.arguments.String()}})
	}
	if c.chat.finishReason == "tool_calls" && len(message.ToolCalls) == 0 {
		return nil, false
	}
	raw, err := json.Marshal(message)
	if err != nil || len(raw) > c.limits.MaxEntryBytes {
		return nil, false
	}
	return raw, true
}
func (c *NativeReasoningChatCapture) bufferedMessageLocked() (json.RawMessage, bool) {
	object, ok := nativeReasoningCaptureObject(c.buffer)
	if !ok {
		return nil, false
	}
	var kind string
	if json.Unmarshal(object["object"], &kind) != nil || kind != "chat.completion" {
		return nil, false
	}
	if _, hasError := object["error"]; hasError {
		return nil, false
	}
	var choices []struct {
		Index        *int            `json:"index"`
		Message      json.RawMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	}
	if json.Unmarshal(object["choices"], &choices) != nil || len(choices) != 1 || choices[0].Index == nil || *choices[0].Index != 0 || !nativeReasoningCompleteChatFinish(choices[0].FinishReason) {
		return nil, false
	}
	raw, err := nativeReplayCanonicalObject(choices[0].Message)
	if err != nil || len(raw) > c.limits.MaxEntryBytes {
		return nil, false
	}
	var message apicompat.ChatMessage
	if json.Unmarshal(raw, &message) != nil || message.Role != "assistant" || message.FunctionCall != nil || len(message.ToolCalls) > c.limits.MaxOutputItems {
		return nil, false
	}
	if len(message.Content) > 0 && !bytes.Equal(message.Content, []byte("null")) {
		var text string
		if json.Unmarshal(message.Content, &text) != nil {
			return nil, false
		}
	}
	seen := map[string]bool{}
	for _, call := range message.ToolCalls {
		if call.ID == "" || seen[call.ID] || call.Type != "function" || call.Function.Name == "" {
			return nil, false
		}
		seen[call.ID] = true
	}
	if choices[0].FinishReason == "tool_calls" && len(message.ToolCalls) == 0 {
		return nil, false
	}
	return raw, true
}
func (c *NativeReasoningChatCapture) DeliveredAssistant() (json.RawMessage, bool) {
	// Serialize with writes before consulting the embedded writer's status.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ResponseWriter == nil || c.ResponseWriter.Status() != 200 {
		return nil, false
	}
	return c.messageLocked()
}

// Preserve the writer's serialization when the streaming handler flushes or
// emits headers while another goroutine takes a delivery snapshot.
func (c *NativeReasoningChatCapture) Flush() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ResponseWriter != nil {
		c.ResponseWriter.Flush()
	}
}
func (c *NativeReasoningChatCapture) WriteHeader(status int) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ResponseWriter != nil {
		c.ResponseWriter.WriteHeader(status)
	}
}
func (c *NativeReasoningChatCapture) WriteHeaderNow() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.ResponseWriter != nil {
		c.ResponseWriter.WriteHeaderNow()
	}
}
