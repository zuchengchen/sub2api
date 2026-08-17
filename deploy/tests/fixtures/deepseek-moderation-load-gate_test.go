package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	deepSeekLoadGateProductionDuration = 10 * time.Minute
	deepSeekLoadGateMinimumRate        = 440
	deepSeekLoadGateDefaultRate        = 480
	deepSeekLoadGateWarmupReviews      = 8
	deepSeekLoadGateDedupeProbeReviews = 80
	deepSeekLoadGateResourceAllowance  = 64 << 20
	deepSeekLoadGateRSSSpreadAllowance = 16 << 20
)

type deepSeekLoadGateReport struct {
	SchemaVersion int                        `json:"schema_version"`
	Status        string                     `json:"status"`
	Mode          string                     `json:"mode"`
	Commit        string                     `json:"commit"`
	StartedAt     string                     `json:"started_at"`
	EndedAt       string                     `json:"ended_at"`
	Configuration deepSeekLoadConfiguration  `json:"configuration"`
	Throughput    deepSeekLoadThroughput     `json:"throughput"`
	Counts        deepSeekLoadCounts         `json:"counts"`
	Stub          deepSeekLoadStubReport     `json:"stub"`
	Resources     deepSeekLoadResourceReport `json:"resources"`
	Checks        deepSeekLoadChecks         `json:"checks"`
	Failures      []string                   `json:"failures,omitempty"`
}

type deepSeekLoadConfiguration struct {
	Model                    string  `json:"model"`
	ThinkingType             string  `json:"thinking_type"`
	ResponseFormat           string  `json:"response_format"`
	FirstLayerStage          string  `json:"first_layer_stage"`
	SecondLayerStage         string  `json:"second_layer_stage"`
	DeepSeekEnabled          bool    `json:"deepseek_enabled"`
	YuFengEnabled            bool    `json:"yufeng_enabled"`
	ChannelTimeoutMS         int     `json:"channel_timeout_ms"`
	TotalTimeoutMS           int     `json:"total_timeout_ms"`
	GOMAXPROCS               int     `json:"gomaxprocs"`
	RequestedDurationSeconds float64 `json:"requested_duration_seconds"`
	TargetReviewsPerMinute   int     `json:"target_reviews_per_minute"`
	EvidenceMode             string  `json:"evidence_mode"`
	DedupeProbeReviews       int     `json:"dedupe_probe_reviews"`
}

type deepSeekLoadThroughput struct {
	MinimumReviewsPerMinute    int     `json:"minimum_reviews_per_minute"`
	CompletedReviewsPerMinute  float64 `json:"completed_reviews_per_minute"`
	SchedulerSeconds           float64 `json:"scheduler_seconds"`
	FirstToLastDispatchSeconds float64 `json:"first_to_last_dispatch_seconds"`
	DrainSeconds               float64 `json:"drain_seconds"`
	TotalSeconds               float64 `json:"total_seconds"`
}

type deepSeekLoadCounts struct {
	WarmupReviews          int64 `json:"warmup_reviews"`
	Scheduled              int64 `json:"scheduled"`
	AllowedSynchronously   int64 `json:"allowed_synchronously"`
	ShadowSubmitted        int64 `json:"shadow_submitted"`
	ShadowCompleted        int64 `json:"shadow_completed"`
	ShadowInFlightAtEnd    int64 `json:"shadow_in_flight_at_end"`
	AuditLogs              int64 `json:"audit_logs"`
	UniqueRequestIDs       int64 `json:"unique_request_ids"`
	UniqueInputHashes      int64 `json:"unique_input_hashes"`
	ExpectedUniqueEvidence int64 `json:"expected_unique_evidence"`
	ProductionPathRecords  int64 `json:"production_path_records"`
	ModelReviewRecords     int64 `json:"model_review_records"`
	CacheHitRecords        int64 `json:"cache_hit_records"`
	CacheHits              int64 `json:"cache_hits"`
	CacheMisses            int64 `json:"cache_misses"`
	CacheWrites            int64 `json:"cache_writes"`
	CacheErrors            int64 `json:"cache_errors"`
	StubRequests           int64 `json:"stub_requests"`
	StubChannelCalls       int64 `json:"stub_channel_calls"`
	ContractViolations     int64 `json:"contract_violations"`
	DedupeProbeScheduled   int64 `json:"dedupe_probe_scheduled"`
	DedupeProbeAllowed     int64 `json:"dedupe_probe_allowed"`
	DedupeProbeAudits      int64 `json:"dedupe_probe_audits"`
	DedupeProbeModelCalls  int64 `json:"dedupe_probe_model_calls"`
	DedupeProbeCacheHits   int64 `json:"dedupe_probe_cache_hits"`
	DedupeProbeCacheMisses int64 `json:"dedupe_probe_cache_misses"`
	DedupeProbeCacheWrites int64 `json:"dedupe_probe_cache_writes"`
	DedupeProbeCacheErrors int64 `json:"dedupe_probe_cache_errors"`
}

type deepSeekLoadStubReport struct {
	InstanceIDBefore string `json:"instance_id_before"`
	InstanceIDAfter  string `json:"instance_id_after"`
	StartedAtBefore  string `json:"started_at_before"`
	StartedAtAfter   string `json:"started_at_after"`
	MaxActive        int    `json:"max_active"`
}

type deepSeekLoadResourceReport struct {
	GoroutinesBaseline int      `json:"goroutines_baseline"`
	GoroutinesEnd      int      `json:"goroutines_end"`
	GoroutinesLimit    int      `json:"goroutines_limit"`
	HeapBaselineBytes  uint64   `json:"heap_baseline_bytes"`
	HeapEndBytes       uint64   `json:"heap_end_bytes"`
	HeapLimitBytes     uint64   `json:"heap_limit_bytes"`
	RSSBaselineBytes   uint64   `json:"rss_baseline_bytes"`
	RSSEndBytes        uint64   `json:"rss_end_bytes"`
	RSSLimitBytes      uint64   `json:"rss_limit_bytes"`
	RSSPeakBytes       uint64   `json:"rss_peak_bytes"`
	RSSTailMinBytes    uint64   `json:"rss_tail_min_bytes"`
	RSSTailMaxBytes    uint64   `json:"rss_tail_max_bytes"`
	RSSTailSpreadBytes uint64   `json:"rss_tail_spread_bytes"`
	RSSSampleCount     int      `json:"rss_sample_count"`
	RSSTailSamples     []uint64 `json:"rss_tail_samples_bytes"`
}

type deepSeekLoadChecks struct {
	ProductionDuration          bool `json:"production_duration"`
	SustainedDispatch           bool `json:"sustained_dispatch"`
	MinimumThroughput           bool `json:"minimum_throughput"`
	ExactCount                  bool `json:"exact_count"`
	ExactDeduplication          bool `json:"exact_deduplication"`
	DifferentEvidenceConcurrent bool `json:"different_evidence_concurrent"`
	ProductionPath              bool `json:"production_path"`
	NoDeadlock                  bool `json:"no_deadlock"`
	NoRestart                   bool `json:"no_restart"`
	ContractValid               bool `json:"contract_valid"`
	GoroutinesBounded           bool `json:"goroutines_bounded"`
	HeapBounded                 bool `json:"heap_bounded"`
	RSSStable                   bool `json:"rss_stable"`
	GOMAXPROCSCorrect           bool `json:"gomaxprocs_correct"`
}

type deepSeekLoadStubStats struct {
	InstanceID         string           `json:"instance_id"`
	StartedAt          string           `json:"started_at"`
	Requests           int64            `json:"requests"`
	ContractViolations int64            `json:"contract_violations"`
	Active             int              `json:"active"`
	MaxActive          int              `json:"max_active"`
	CallsByChannel     map[string]int64 `json:"calls_by_channel"`
}

type deepSeekLoadResourceSnapshot struct {
	Goroutines int
	HeapBytes  uint64
	RSSBytes   uint64
}

type deepSeekLoadLogInspection struct {
	Count                 int64
	UniqueRequestIDs      int64
	UniqueInputHashes     int64
	ProductionPathRecords int64
	ModelReviewRecords    int64
	CacheHitRecords       int64
}

func TestDeepSeekModerationSustainedLoadGate(t *testing.T) {
	report := deepSeekLoadGateReport{
		SchemaVersion: 1,
		Status:        "failed",
		Commit:        strings.TrimSpace(os.Getenv("SUB2API_DEEPSEEK_LOAD_COMMIT")),
		StartedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	reportPath := strings.TrimSpace(os.Getenv("SUB2API_DEEPSEEK_LOAD_REPORT"))
	failures := runDeepSeekModerationLoadGate(&report)
	report.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
	report.Failures = failures
	if len(failures) == 0 {
		report.Status = "passed"
	}
	if err := writeDeepSeekLoadGateReport(reportPath, report); err != nil {
		t.Fatalf("write load gate report: %v", err)
	}
	if len(failures) > 0 {
		t.Errorf("DeepSeek moderation load gate failed: %s", strings.Join(failures, "; "))
	}
}

func runDeepSeekModerationLoadGate(report *deepSeekLoadGateReport) []string {
	var failures []string
	stubURL := strings.TrimRight(strings.TrimSpace(os.Getenv("SUB2API_DEEPSEEK_LOAD_STUB_URL")), "/")
	if stubURL == "" {
		return []string{"SUB2API_DEEPSEEK_LOAD_STUB_URL is required"}
	}
	duration, err := deepSeekLoadGateDurationEnv("SUB2API_DEEPSEEK_LOAD_DURATION", deepSeekLoadGateProductionDuration)
	if err != nil {
		return []string{err.Error()}
	}
	drainTimeout, err := deepSeekLoadGateDurationEnv("SUB2API_DEEPSEEK_LOAD_DRAIN_TIMEOUT", 2*time.Minute)
	if err != nil {
		return []string{err.Error()}
	}
	rate, err := deepSeekLoadGateIntEnv("SUB2API_DEEPSEEK_LOAD_RATE_PER_MINUTE", deepSeekLoadGateDefaultRate)
	if err != nil {
		return []string{err.Error()}
	}
	allowShort, err := deepSeekLoadGateBoolEnv("SUB2API_DEEPSEEK_LOAD_ALLOW_SHORT", false)
	if err != nil {
		return []string{err.Error()}
	}
	if duration < time.Second {
		failures = append(failures, "load duration must be at least one second")
	}
	if duration < deepSeekLoadGateProductionDuration && !allowShort {
		failures = append(failures, "durations below 10 minutes require SUB2API_DEEPSEEK_LOAD_ALLOW_SHORT=true")
	}
	if rate < deepSeekLoadGateMinimumRate || rate > 5000 {
		failures = append(failures, fmt.Sprintf("review rate must be between %d and 5000 per minute", deepSeekLoadGateMinimumRate))
	}
	if drainTimeout < 5*time.Second {
		failures = append(failures, "drain timeout must be at least five seconds")
	}
	if len(failures) > 0 {
		return failures
	}

	report.Mode = "production"
	if duration < deepSeekLoadGateProductionDuration {
		report.Mode = "focused"
	}
	report.Configuration = deepSeekLoadConfiguration{
		Model: DefaultContentModerationDeepSeekModel, ThinkingType: "disabled", ResponseFormat: "json_object",
		FirstLayerStage: ContentModerationFirstLayerStageShadow, SecondLayerStage: ContentModerationSecondLayerStageShadow,
		DeepSeekEnabled: true, YuFengEnabled: false,
		ChannelTimeoutMS: DefaultContentModerationDeepSeekChannelTimeoutMS,
		TotalTimeoutMS:   DefaultContentModerationDeepSeekTotalTimeoutMS,
		GOMAXPROCS:       runtime.GOMAXPROCS(0), RequestedDurationSeconds: duration.Seconds(),
		TargetReviewsPerMinute: rate, EvidenceMode: "unique_per_request",
		DedupeProbeReviews: deepSeekLoadGateDedupeProbeReviews,
	}
	report.Throughput.MinimumReviewsPerMinute = deepSeekLoadGateMinimumRate
	report.Checks.ProductionDuration = duration >= deepSeekLoadGateProductionDuration
	if report.Mode == "focused" {
		report.Checks.ProductionDuration = false
	}
	report.Checks.GOMAXPROCSCorrect = report.Configuration.GOMAXPROCS == 28
	if !report.Checks.GOMAXPROCSCorrect {
		failures = append(failures, fmt.Sprintf("GOMAXPROCS=%d, want 28", report.Configuration.GOMAXPROCS))
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	initialStub, err := fetchDeepSeekLoadStubStats(httpClient, stubURL)
	if err != nil {
		return append(failures, fmt.Sprintf("read initial stub stats: %v", err))
	}

	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.DeepSeekEnabled = true
	cfg.YuFengEnabled = false
	cfg.DeepSeekThreshold = DefaultContentModerationDeepSeekThreshold
	cfg.DeepSeekTotalTimeoutMS = DefaultContentModerationDeepSeekTotalTimeoutMS
	cfg.DeepSeekChannels = []ContentModerationDeepSeekChannel{{
		ID: "load", Name: "load", BaseURL: stubURL + "/load", Model: DefaultContentModerationDeepSeekModel,
		Enabled: true, Order: 0, TimeoutMS: DefaultContentModerationDeepSeekChannelTimeoutMS, APIKey: "load-test-key",
	}}
	cfg.FirstLayerStage = ContentModerationFirstLayerStageShadow
	cfg.SecondLayerEnabled = true
	cfg.SecondLayerStage = ContentModerationSecondLayerStageShadow
	cfg.SecondLayerEndpoints = nil
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordAndAPI
	cfg.BlockedKeywords = nil
	cfg.HardBlockPatterns = nil
	cfg.CandidateKeywords = []string{"SQL注入"}
	cfg.RecordNonHits = true
	cfg.AutoBanEnabled = false
	cfg.EmailOnHit = false
	cfg.normalize()
	runtimeSnapshot := &contentModerationRuntimeSnapshot{
		riskControlEnabled:          true,
		config:                      cfg,
		secondLayerPrefilterMatcher: newContentModerationPrefilterMatcher(cfg.CandidateKeywords),
		fragmentCacheNamespace:      cfg.fragmentCacheNamespace(),
	}
	repo := &contentModerationTestRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)

	interval := time.Minute / time.Duration(rate)
	warmupAllowed := int64(0)
	for index := 0; index < deepSeekLoadGateWarmupReviews; index++ {
		if decision := svc.checkUnifiedFragments(context.Background(), deepSeekLoadGateInput("warmup", int64(index), deepSeekLoadGateWarmupReviews), runtimeSnapshot); decision != nil && decision.Allowed && !decision.Blocked {
			warmupAllowed++
		}
		if index+1 < deepSeekLoadGateWarmupReviews {
			time.Sleep(interval)
		}
	}
	warmupDrained, _ := waitForDeepSeekLoadDrain(svc, 30*time.Second)
	if warmupAllowed != deepSeekLoadGateWarmupReviews || !warmupDrained || svc.secondLayerShadowSubmitted.Load() != deepSeekLoadGateWarmupReviews {
		return append(failures, "production review path warmup did not complete exactly")
	}
	report.Counts.WarmupReviews = deepSeekLoadGateWarmupReviews

	baselineStub, err := fetchDeepSeekLoadStubStats(httpClient, stubURL)
	if err != nil {
		return append(failures, fmt.Sprintf("read post-warmup stub stats: %v", err))
	}
	if baselineStub.InstanceID == "" || baselineStub.InstanceID != initialStub.InstanceID || baselineStub.StartedAt != initialStub.StartedAt {
		return append(failures, "stub restarted during warmup")
	}
	probeSubmittedBaseline := svc.secondLayerShadowSubmitted.Load()
	probeCompletedBaseline := svc.secondLayerShadowCompleted.Load()
	probeLogsBaseline := deepSeekLoadGateRepoLogCount(repo)
	probeCacheHitsBaseline := svc.secondLayerCacheHits.Load()
	probeCacheMissesBaseline := svc.secondLayerCacheMisses.Load()
	probeCacheWritesBaseline := svc.secondLayerCacheWrites.Load()
	probeCacheErrorsBaseline := svc.secondLayerCacheErrors.Load()
	var probeAllowed int64
	for index := int64(0); index < deepSeekLoadGateDedupeProbeReviews; index++ {
		decision := svc.checkUnifiedFragments(
			context.Background(), deepSeekLoadGateInput("dedupe", index, 1), runtimeSnapshot,
		)
		if decision != nil && decision.Allowed && !decision.Blocked {
			probeAllowed++
		}
	}
	probeDrained, _ := waitForDeepSeekLoadDrain(svc, 30*time.Second)
	postProbeStub, err := fetchDeepSeekLoadStubStats(httpClient, stubURL)
	if err != nil {
		return append(failures, fmt.Sprintf("read post-deduplication-probe stub stats: %v", err))
	}
	probeInspection := inspectDeepSeekLoadGateLogs(repo, probeLogsBaseline, "dedupe-review-")
	probeSubmitted := svc.secondLayerShadowSubmitted.Load() - probeSubmittedBaseline
	probeCompleted := svc.secondLayerShadowCompleted.Load() - probeCompletedBaseline
	probeCacheHits := svc.secondLayerCacheHits.Load() - probeCacheHitsBaseline
	probeCacheMisses := svc.secondLayerCacheMisses.Load() - probeCacheMissesBaseline
	probeCacheWrites := svc.secondLayerCacheWrites.Load() - probeCacheWritesBaseline
	probeCacheErrors := svc.secondLayerCacheErrors.Load() - probeCacheErrorsBaseline
	probeModelCalls := postProbeStub.Requests - baselineStub.Requests
	report.Counts.DedupeProbeScheduled = deepSeekLoadGateDedupeProbeReviews
	report.Counts.DedupeProbeAllowed = probeAllowed
	report.Counts.DedupeProbeAudits = probeInspection.Count
	report.Counts.DedupeProbeModelCalls = probeModelCalls
	report.Counts.DedupeProbeCacheHits = probeCacheHits
	report.Counts.DedupeProbeCacheMisses = probeCacheMisses
	report.Counts.DedupeProbeCacheWrites = probeCacheWrites
	report.Counts.DedupeProbeCacheErrors = probeCacheErrors
	dedupeProbePassed := probeDrained && probeAllowed == deepSeekLoadGateDedupeProbeReviews &&
		probeSubmitted == deepSeekLoadGateDedupeProbeReviews && probeCompleted == deepSeekLoadGateDedupeProbeReviews &&
		probeInspection.Count == deepSeekLoadGateDedupeProbeReviews &&
		probeInspection.UniqueRequestIDs == deepSeekLoadGateDedupeProbeReviews && probeInspection.UniqueInputHashes == 1 &&
		probeInspection.ModelReviewRecords == 1 && probeInspection.CacheHitRecords == deepSeekLoadGateDedupeProbeReviews-1 &&
		probeCacheHits == deepSeekLoadGateDedupeProbeReviews-1 && probeCacheMisses == 1 && probeCacheWrites == 1 &&
		probeCacheErrors == 0 && probeModelCalls == 1 &&
		postProbeStub.CallsByChannel["load"]-baselineStub.CallsByChannel["load"] == 1
	if !dedupeProbePassed {
		failures = append(failures, "concurrent identical-evidence deduplication probe failed")
	}
	baselineStub = postProbeStub
	baselineSubmitted := svc.secondLayerShadowSubmitted.Load()
	baselineCompleted := svc.secondLayerShadowCompleted.Load()
	baselineLogs := deepSeekLoadGateRepoLogCount(repo)
	baselineCacheHits := svc.secondLayerCacheHits.Load()
	baselineCacheMisses := svc.secondLayerCacheMisses.Load()
	baselineCacheWrites := svc.secondLayerCacheWrites.Load()
	baselineCacheErrors := svc.secondLayerCacheErrors.Load()
	baselineResource, _, err := collectDeepSeekLoadSettledResources(3)
	if err != nil {
		return append(failures, fmt.Sprintf("collect baseline resources: %v", err))
	}
	report.Resources.GoroutinesBaseline = baselineResource.Goroutines
	report.Resources.HeapBaselineBytes = baselineResource.HeapBytes
	report.Resources.RSSBaselineBytes = baselineResource.RSSBytes

	stopRSSMonitor, rssResult := startDeepSeekLoadRSSMonitor()
	loadStarted := time.Now()
	steps := int64((duration + interval - 1) / interval)
	expected := steps + 1
	var scheduled int64
	var allowed int64
	firstDispatch := loadStarted
	var lastDispatch time.Time
	for index := int64(0); index < expected; index++ {
		offset := time.Duration(index) * interval
		if offset > duration {
			offset = duration
		}
		if wait := time.Until(loadStarted.Add(offset)); wait > 0 {
			time.Sleep(wait)
		}
		dispatchedAt := time.Now()
		lastDispatch = dispatchedAt
		decision := svc.checkUnifiedFragments(context.Background(), deepSeekLoadGateInput("load", index, 0), runtimeSnapshot)
		scheduled++
		if decision != nil && decision.Allowed && !decision.Blocked {
			allowed++
		}
	}
	schedulerEnded := time.Now()
	drainStarted := time.Now()
	drained, drainEnded := waitForDeepSeekLoadDrain(svc, drainTimeout)
	stopRSSMonitor()
	rssSamples := <-rssResult

	finalStub, stubErr := fetchDeepSeekLoadStubStats(httpClient, stubURL)
	if stubErr != nil {
		failures = append(failures, fmt.Sprintf("read final stub stats: %v", stubErr))
		finalStub = baselineStub
	}
	submitted := svc.secondLayerShadowSubmitted.Load() - baselineSubmitted
	completed := svc.secondLayerShadowCompleted.Load() - baselineCompleted
	inFlight := svc.secondLayerShadowInFlight.Load()
	inspection := inspectDeepSeekLoadGateLogs(repo, baselineLogs, "load-review-")
	cacheHits := svc.secondLayerCacheHits.Load() - baselineCacheHits
	cacheMisses := svc.secondLayerCacheMisses.Load() - baselineCacheMisses
	cacheWrites := svc.secondLayerCacheWrites.Load() - baselineCacheWrites
	cacheErrors := svc.secondLayerCacheErrors.Load() - baselineCacheErrors
	stubRequests := finalStub.Requests - baselineStub.Requests
	stubCalls := finalStub.CallsByChannel["load"] - baselineStub.CallsByChannel["load"]
	contractViolations := finalStub.ContractViolations - baselineStub.ContractViolations

	totalSeconds := drainEnded.Sub(loadStarted).Seconds()
	completedRate := float64(completed) / totalSeconds * 60
	report.Throughput.CompletedReviewsPerMinute = completedRate
	report.Throughput.SchedulerSeconds = schedulerEnded.Sub(loadStarted).Seconds()
	report.Throughput.FirstToLastDispatchSeconds = lastDispatch.Sub(firstDispatch).Seconds()
	report.Throughput.DrainSeconds = drainEnded.Sub(drainStarted).Seconds()
	report.Throughput.TotalSeconds = totalSeconds
	report.Counts.Scheduled = scheduled
	report.Counts.AllowedSynchronously = allowed
	report.Counts.ShadowSubmitted = submitted
	report.Counts.ShadowCompleted = completed
	report.Counts.ShadowInFlightAtEnd = inFlight
	report.Counts.AuditLogs = inspection.Count
	report.Counts.UniqueRequestIDs = inspection.UniqueRequestIDs
	report.Counts.UniqueInputHashes = inspection.UniqueInputHashes
	report.Counts.ProductionPathRecords = inspection.ProductionPathRecords
	report.Counts.ModelReviewRecords = inspection.ModelReviewRecords
	report.Counts.CacheHitRecords = inspection.CacheHitRecords
	report.Counts.CacheHits = cacheHits
	report.Counts.CacheMisses = cacheMisses
	report.Counts.CacheWrites = cacheWrites
	report.Counts.CacheErrors = cacheErrors
	report.Counts.StubRequests = stubRequests
	report.Counts.StubChannelCalls = stubCalls
	report.Counts.ContractViolations = contractViolations
	report.Stub = deepSeekLoadStubReport{
		InstanceIDBefore: baselineStub.InstanceID, InstanceIDAfter: finalStub.InstanceID,
		StartedAtBefore: baselineStub.StartedAt, StartedAtAfter: finalStub.StartedAt,
		MaxActive: finalStub.MaxActive,
	}

	report.Checks.SustainedDispatch = schedulerEnded.Sub(loadStarted) >= duration && lastDispatch.Sub(firstDispatch) >= duration
	report.Checks.MinimumThroughput = completedRate >= deepSeekLoadGateMinimumRate
	expectedUniqueEvidence := scheduled
	expectedCacheHits := int64(0)
	report.Counts.ExpectedUniqueEvidence = expectedUniqueEvidence
	report.Checks.ExactCount = allowed == scheduled && submitted == scheduled && completed == scheduled &&
		inspection.Count == scheduled && inFlight == 0
	report.Checks.ExactDeduplication = dedupeProbePassed && inspection.UniqueRequestIDs == scheduled &&
		inspection.UniqueInputHashes == expectedUniqueEvidence && inspection.ModelReviewRecords == expectedUniqueEvidence &&
		inspection.CacheHitRecords == expectedCacheHits && cacheHits == expectedCacheHits &&
		cacheMisses == expectedUniqueEvidence && cacheWrites == expectedUniqueEvidence &&
		cacheErrors == 0 && stubRequests == expectedUniqueEvidence && stubCalls == expectedUniqueEvidence
	report.Checks.DifferentEvidenceConcurrent = finalStub.MaxActive >= 2
	report.Checks.ProductionPath = inspection.ProductionPathRecords == scheduled
	report.Checks.NoDeadlock = drained && inFlight == 0
	report.Checks.NoRestart = baselineStub.InstanceID != "" && baselineStub.InstanceID == finalStub.InstanceID &&
		baselineStub.StartedAt != "" && baselineStub.StartedAt == finalStub.StartedAt
	report.Checks.ContractValid = contractViolations == 0 && finalStub.Active == 0

	endResource, rssTail, resourceErr := collectDeepSeekLoadSettledResources(5)
	runtime.KeepAlive(svc)
	runtime.KeepAlive(repo)
	if resourceErr != nil {
		failures = append(failures, fmt.Sprintf("collect ending resources: %v", resourceErr))
	} else {
		heapLimit := baselineResource.HeapBytes + baselineResource.HeapBytes/5 + deepSeekLoadGateResourceAllowance
		rssLimit := baselineResource.RSSBytes + baselineResource.RSSBytes/5 + deepSeekLoadGateResourceAllowance
		rssMin, rssMax := deepSeekLoadGateMinMax(rssTail)
		report.Resources.GoroutinesEnd = endResource.Goroutines
		report.Resources.GoroutinesLimit = baselineResource.Goroutines + 10
		report.Resources.HeapEndBytes = endResource.HeapBytes
		report.Resources.HeapLimitBytes = heapLimit
		report.Resources.RSSEndBytes = endResource.RSSBytes
		report.Resources.RSSLimitBytes = rssLimit
		report.Resources.RSSPeakBytes = deepSeekLoadGateMaxRSS(rssSamples, rssTail)
		report.Resources.RSSTailMinBytes = rssMin
		report.Resources.RSSTailMaxBytes = rssMax
		report.Resources.RSSTailSpreadBytes = rssMax - rssMin
		report.Resources.RSSSampleCount = len(rssSamples)
		report.Resources.RSSTailSamples = rssTail
		report.Checks.GoroutinesBounded = endResource.Goroutines <= baselineResource.Goroutines+10
		report.Checks.HeapBounded = endResource.HeapBytes <= heapLimit
		report.Checks.RSSStable = endResource.RSSBytes <= rssLimit && rssMax-rssMin <= deepSeekLoadGateRSSSpreadAllowance
	}

	if report.Mode == "production" && !report.Checks.ProductionDuration {
		failures = append(failures, "production run did not request at least ten minutes")
	}
	for name, passed := range map[string]bool{
		"sustained dispatch":             report.Checks.SustainedDispatch,
		"minimum throughput":             report.Checks.MinimumThroughput,
		"exact count":                    report.Checks.ExactCount,
		"exact deduplication":            report.Checks.ExactDeduplication,
		"different evidence concurrency": report.Checks.DifferentEvidenceConcurrent,
		"production path":                report.Checks.ProductionPath,
		"no deadlock":                    report.Checks.NoDeadlock,
		"no restart":                     report.Checks.NoRestart,
		"request contract":               report.Checks.ContractValid,
		"goroutine bound":                report.Checks.GoroutinesBounded,
		"heap bound":                     report.Checks.HeapBounded,
		"RSS stability":                  report.Checks.RSSStable,
	} {
		if !passed {
			failures = append(failures, name+" check failed")
		}
	}
	return failures
}

func deepSeekLoadGateInput(prefix string, index int64, uniqueEvidence int) ContentModerationCheckInput {
	scope := NewContentModerationScopeSnapshot(nil, "gpt-deepseek-load")
	variant := index
	if uniqueEvidence > 0 {
		variant %= int64(uniqueEvidence)
	}
	text := fmt.Sprintf("自有系统 SQL注入 防御审计负载样本 %s-%08d", prefix, variant)
	return ContentModerationCheckInput{
		RequestID: fmt.Sprintf("%s-review-%08d", prefix, index),
		UserID:    8077, UserRole: RoleUser, GroupName: scope.GroupName,
		Scope: &scope, Protocol: ContentModerationProtocolOpenAIResponses,
		Body: []byte(`{"input":` + strconv.Quote(text) + `}`),
	}
}

func waitForDeepSeekLoadDrain(svc *ContentModerationService, timeout time.Duration) (bool, time.Time) {
	deadline := time.Now().Add(timeout)
	for {
		submitted := svc.secondLayerShadowSubmitted.Load()
		completed := svc.secondLayerShadowCompleted.Load()
		inFlight := svc.secondLayerShadowInFlight.Load()
		if completed == submitted && inFlight == 0 {
			return true, time.Now()
		}
		if time.Now().After(deadline) {
			return false, time.Now()
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func deepSeekLoadGateRepoLogCount(repo *contentModerationTestRepo) int {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	return len(repo.logs)
}

func inspectDeepSeekLoadGateLogs(repo *contentModerationTestRepo, start int, requestIDPrefix string) deepSeekLoadLogInspection {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if start < 0 || start > len(repo.logs) {
		return deepSeekLoadLogInspection{}
	}
	requestIDs := make(map[string]struct{}, len(repo.logs)-start)
	inputHashes := make(map[string]struct{}, len(repo.logs)-start)
	var productionRecords int64
	var modelReviewRecords int64
	var cacheHitRecords int64
	for index := start; index < len(repo.logs); index++ {
		log := repo.logs[index]
		if strings.HasPrefix(log.RequestID, requestIDPrefix) {
			requestIDs[log.RequestID] = struct{}{}
		}
		if log.InputHash != "" {
			inputHashes[log.InputHash] = struct{}{}
		}
		modelReview := log.DecisionSource == "model_shadow" && !log.CacheHit
		cacheReplay := log.DecisionSource == "cache_replay" && log.CacheHit
		modelReviewValid := modelReview && len(log.ReviewAttempts) == 1 &&
			log.ReviewAttempts[0].Reviewer == "deepseek" &&
			log.ReviewAttempts[0].ChannelID == "load" && log.ReviewAttempts[0].Outcome == "success"
		cacheReplayValid := cacheReplay && len(log.ReviewAttempts) == 0
		if log.Action == ContentModerationActionSecondLayerShadow && (modelReviewValid || cacheReplayValid) &&
			log.DeepSeekCategory == "safe" && log.ReviewOutcome == "safe" {
			productionRecords++
			if modelReview {
				modelReviewRecords++
			}
			if cacheReplay {
				cacheHitRecords++
			}
		}
	}
	return deepSeekLoadLogInspection{
		Count: int64(len(repo.logs) - start), UniqueRequestIDs: int64(len(requestIDs)),
		UniqueInputHashes: int64(len(inputHashes)), ProductionPathRecords: productionRecords,
		ModelReviewRecords: modelReviewRecords, CacheHitRecords: cacheHitRecords,
	}
}

func fetchDeepSeekLoadStubStats(client *http.Client, stubURL string) (deepSeekLoadStubStats, error) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, stubURL+"/stats", nil)
	if err != nil {
		return deepSeekLoadStubStats{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return deepSeekLoadStubStats{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return deepSeekLoadStubStats{}, fmt.Errorf("stub stats HTTP status %d", response.StatusCode)
	}
	var stats deepSeekLoadStubStats
	if err := json.NewDecoder(response.Body).Decode(&stats); err != nil {
		return deepSeekLoadStubStats{}, err
	}
	if stats.CallsByChannel == nil {
		stats.CallsByChannel = map[string]int64{}
	}
	return stats, nil
}

func startDeepSeekLoadRSSMonitor() (func(), <-chan []uint64) {
	stop := make(chan struct{})
	result := make(chan []uint64, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		samples := make([]uint64, 0, 601)
		if rss, err := deepSeekLoadGateRSSBytes(); err == nil {
			samples = append(samples, rss)
		}
		for {
			select {
			case <-ticker.C:
				if rss, err := deepSeekLoadGateRSSBytes(); err == nil {
					samples = append(samples, rss)
				}
			case <-stop:
				result <- samples
				return
			}
		}
	}()
	return func() { close(stop) }, result
}

func collectDeepSeekLoadSettledResources(samples int) (deepSeekLoadResourceSnapshot, []uint64, error) {
	rssTail := make([]uint64, 0, samples)
	for range samples {
		debug.FreeOSMemory()
		time.Sleep(200 * time.Millisecond)
		rss, err := deepSeekLoadGateRSSBytes()
		if err != nil {
			return deepSeekLoadResourceSnapshot{}, rssTail, err
		}
		rssTail = append(rssTail, rss)
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return deepSeekLoadResourceSnapshot{
		Goroutines: runtime.NumGoroutine(), HeapBytes: memory.HeapAlloc, RSSBytes: rssTail[len(rssTail)-1],
	}, rssTail, nil
}

func deepSeekLoadGateRSSBytes() (uint64, error) {
	contents, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(contents), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kilobytes * 1024, nil
	}
	return 0, fmt.Errorf("VmRSS is unavailable in /proc/self/status")
}

func deepSeekLoadGateMinMax(values []uint64) (uint64, uint64) {
	if len(values) == 0 {
		return 0, 0
	}
	minimum, maximum := values[0], values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
		if value > maximum {
			maximum = value
		}
	}
	return minimum, maximum
}

func deepSeekLoadGateMaxRSS(groups ...[]uint64) uint64 {
	var maximum uint64
	for _, group := range groups {
		_, candidate := deepSeekLoadGateMinMax(group)
		if candidate > maximum {
			maximum = candidate
		}
	}
	return maximum
}

func deepSeekLoadGateDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return parsed, nil
}

func deepSeekLoadGateIntEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return parsed, nil
}

func deepSeekLoadGateBoolEnv(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return parsed, nil
}

func writeDeepSeekLoadGateReport(path string, report deepSeekLoadGateReport) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("SUB2API_DEEPSEEK_LOAD_REPORT is required")
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(report)
	syncErr := file.Sync()
	closeErr := file.Close()
	if encodeErr != nil {
		_ = os.Remove(temporary)
		return encodeErr
	}
	if syncErr != nil {
		_ = os.Remove(temporary)
		return syncErr
	}
	if closeErr != nil {
		_ = os.Remove(temporary)
		return closeErr
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return os.Chmod(path, 0600)
}
