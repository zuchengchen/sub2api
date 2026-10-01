package service

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAINativeCompactionMissingItemCode    = "native_compaction_missing_item"
	openAINativeCompactionMissingItemMessage = "upstream completed a native remote compaction v2 turn without a compaction output item"
)

// openAINativeCompactionMinStreamInterval is the minimum upstream-silence
// budget for a native remote compaction v2 stream. Summarising a large context
// at high reasoning effort legitimately produces no SSE event between
// response.in_progress and the compaction item for several minutes (observed
// 110-220s on gpt-6-astra). The generic 180s stream_data_interval_timeout cut
// those turns off and wrote a bare "error" frame that Codex ignores, so the
// turn failed after retries. Codex itself waits 300s per SSE event
// (stream_idle_timeout_ms) and SSE comment keepalives do not reset that, so
// the gateway must never give up before the client does.
var openAINativeCompactionMinStreamInterval = 600 * time.Second

// openAINativeCompactionStreamInterval raises the upstream-silence timeout for
// native v2 compaction turns. A disabled timeout (0) stays disabled, and
// ordinary turns keep the configured value.
func openAINativeCompactionStreamInterval(c *gin.Context, configured time.Duration) time.Duration {
	if configured <= 0 || !isOpenAINativeCompactionV2(c) {
		return configured
	}
	if configured < openAINativeCompactionMinStreamInterval {
		return openAINativeCompactionMinStreamInterval
	}
	return configured
}

// openAINativeCompactionTerminalMissingItem reports whether a terminal
// response.completed / response.done payload of a native remote compaction v2
// turn carries no output item at all. Codex requires exactly one compaction
// item and fails the turn fatally when it collects zero ("remote compaction
// v2 expected exactly one compaction output item, got 0 from 0 output
// items"), so such a terminal must never reach the client as a success.
//
// Callers only consult this before any semantic output was seen: a streamed
// compaction (or any other) item already counts as client output, so this
// check is limited to the zero-item shape and never rewrites a turn that
// produced something.
func openAINativeCompactionTerminalMissingItem(c *gin.Context, eventType string, data []byte) bool {
	if !isOpenAINativeCompactionV2(c) {
		return false
	}
	if eventType != "response.completed" && eventType != "response.done" {
		return false
	}
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return false
	}
	if gjson.GetBytes(data, "error").Exists() || gjson.GetBytes(data, "response.error").Type == gjson.JSON {
		return false
	}
	return len(gjson.GetBytes(data, "response.output").Array()) == 0
}

// newOpenAINativeCompactionMissingItemFailoverError turns an empty native v2
// compaction terminal into a retryable upstream anomaly. Compaction carries
// no tool side effects, so another account may safely run it again.
func newOpenAINativeCompactionMissingItemFailoverError(c *gin.Context, account *Account, upstreamRequestID string) *UpstreamFailoverError {
	accountID := int64(0)
	accountName := ""
	platform := PlatformOpenAI
	if account != nil {
		accountID = account.ID
		accountName = account.Name
		platform = account.Platform
	}

	setOpsUpstreamError(c, http.StatusBadGateway, openAINativeCompactionMissingItemMessage, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           platform,
		AccountID:          accountID,
		AccountName:        accountName,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  upstreamRequestID,
		Kind:               "failover",
		Reason:             openAINativeCompactionMissingItemCode,
		Message:            openAINativeCompactionMissingItemMessage,
	})

	headers := http.Header{}
	if strings.TrimSpace(upstreamRequestID) != "" {
		headers.Set("x-request-id", strings.TrimSpace(upstreamRequestID))
	}
	body := []byte(`{"error":{"type":"upstream_error","code":"` + openAINativeCompactionMissingItemCode + `","message":"` + openAINativeCompactionMissingItemMessage + `"}}`)
	return &UpstreamFailoverError{
		StatusCode:      http.StatusBadGateway,
		ResponseBody:    body,
		ResponseHeaders: headers,
	}
}

// excelBPSNativeCompactionHold buffers a native v2 compaction stream from the
// BPS bridge until its terminal event proves that a compaction item exists.
// While held, nothing but keepalive comments reaches the client, so a missing
// item can still fall back to the account's Codex HTTP route.
type excelBPSNativeCompactionHold struct {
	lines     []string
	size      int
	foundItem bool
}

// excelBPSNativeCompactionHoldLimit bounds the held stream. A compaction
// response is a single encrypted item; anything larger is released as-is.
const excelBPSNativeCompactionHoldLimit = 8 << 20

func (h *excelBPSNativeCompactionHold) observe(kind string, payload []byte) {
	switch kind {
	case "response.output_item.added", "response.output_item.done":
		if isResponsesCompactionItemType(gjson.GetBytes(payload, "item.type").String()) {
			h.foundItem = true
		}
	case "response.completed", "response.done":
		if responsesOutputHasCompactionItem([]byte(gjson.GetBytes(payload, "response").Raw)) {
			h.foundItem = true
		}
	}
}

// add stores one line; it returns false when the hold limit is exceeded and
// the caller must release the buffer and stream directly from then on.
func (h *excelBPSNativeCompactionHold) add(line string) bool {
	h.lines = append(h.lines, line)
	h.size += len(line) + 1
	return h.size <= excelBPSNativeCompactionHoldLimit
}

// release writes every held line in order and empties the buffer.
func (h *excelBPSNativeCompactionHold) release(c *gin.Context) error {
	for _, line := range h.lines {
		if _, err := c.Writer.WriteString(line + "\n"); err != nil {
			h.lines = nil
			return err
		}
	}
	h.lines = nil
	c.Writer.Flush()
	return nil
}
