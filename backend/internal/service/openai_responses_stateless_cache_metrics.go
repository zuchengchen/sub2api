package service

import (
	"maps"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// ResponsesStatelessCacheOutcome captures the last Forward attempt before the
// usage worker takes ownership of the result. Publish is called once when the
// logical HTTP request finishes, including failover and early returns.
type ResponsesStatelessCacheOutcome struct {
	assignment string
	action     string
	transport  string
	failed     bool
	usage      OpenAIUsage
	firstMS    int
	hasFirst   bool
}

func (o *ResponsesStatelessCacheOutcome) Capture(c *gin.Context, result *OpenAIForwardResult, err error) {
	assignment, action := responsesStatelessCacheTrialObservation(c)
	if assignment == "" {
		return
	}
	*o = ResponsesStatelessCacheOutcome{assignment: assignment, action: action, transport: "unknown", failed: err != nil || result == nil}
	if result == nil {
		return
	}
	o.transport = "http"
	if result.OpenAIWSMode {
		o.transport = "ws"
	}
	o.usage = result.Usage
	if result.FirstTokenMs != nil && *result.FirstTokenMs >= 0 {
		o.firstMS, o.hasFirst = *result.FirstTokenMs, true
	}
	o.failed = o.failed || result.ClientDisconnect
	switch result.UpstreamTerminalEvent {
	case "", "response.completed", "response.done":
	default:
		o.failed = true
	}
}

func (o ResponsesStatelessCacheOutcome) AttachDiagnostic(d *ResponsesCacheDiagnostic) {
	if d == nil || o.assignment == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished {
		return
	}
	record := map[string]any{"assignment": o.assignment, "final_transport": o.transport, "failed": o.failed}
	if o.action != "" {
		record["action"] = o.action
	}
	if o.hasFirst {
		record["first_token_ms"] = o.firstMS
	}
	d.event["stateless_cache_trial"] = record
}

type responsesCacheTrialTotals struct {
	Requests       int64 `json:"requests"`
	Failures       int64 `json:"failures"`
	PrewarmWritten int64 `json:"prewarm_written"`
	FullWritten    int64 `json:"full_written"`
	FinalWS        int64 `json:"final_ws"`
	Metered        int64 `json:"metered"`
	Input          int64 `json:"input_tokens"`
	Cached         int64 `json:"cached_tokens"`
	FirstTokenN    int64 `json:"first_token_samples"`
	FirstTokenMS   int64 `json:"first_token_ms_sum"`
}

// There are two randomized arms and four fixed length bands. The operator
// enables one key per process. Cumulative snapshots contain only counters.
var statelessTrialMeters struct {
	sync.Mutex
	buckets map[string]responsesCacheTrialTotals
	events  int
	lastLog time.Time
}

func (o ResponsesStatelessCacheOutcome) Publish() {
	if o.assignment != "control_prewarm" && o.assignment != "trial_full" {
		return
	}
	input, cached := o.usage.InputTokens, o.usage.CacheReadInputTokens
	band := "zero_or_unreported"
	switch {
	case input >= 262144:
		band = "262144_plus"
	case input >= 131072:
		band = "131072_262143"
	case input > 0:
		band = "under_131072"
	}
	key := o.assignment + "/" + band
	statelessTrialMeters.Lock()
	if statelessTrialMeters.buckets == nil {
		statelessTrialMeters.buckets = make(map[string]responsesCacheTrialTotals, 8)
	}
	b := statelessTrialMeters.buckets[key]
	b.Requests++
	if o.failed {
		b.Failures++
	}
	switch o.action {
	case "prewarm_written":
		b.PrewarmWritten++
	case "full_input_written":
		b.FullWritten++
	}
	if o.transport == "ws" {
		b.FinalWS++
	}
	if input > 0 && cached >= 0 && o.usage.CacheCreationInputTokens >= 0 && cached <= input-o.usage.CacheCreationInputTokens {
		b.Metered++
		b.Input += int64(input)
		b.Cached += int64(cached)
	}
	if o.hasFirst {
		b.FirstTokenN++
		b.FirstTokenMS += int64(o.firstMS)
	}
	statelessTrialMeters.buckets[key] = b
	statelessTrialMeters.events++
	if statelessTrialMeters.events%10 != 0 && time.Since(statelessTrialMeters.lastLog) < time.Minute {
		statelessTrialMeters.Unlock()
		return
	}
	statelessTrialMeters.lastLog = time.Now()
	snapshot := maps.Clone(statelessTrialMeters.buckets)
	events := statelessTrialMeters.events
	statelessTrialMeters.Unlock()
	logger.L().Info("responses_stateless_cache_trial_totals", zap.Int("events", events), zap.Any("buckets", snapshot))
}
