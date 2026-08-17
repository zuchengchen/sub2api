package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	historySchema             = "sub2api-deepseek-history-evaluation-v1"
	fixedHistoryEligible      = 10879
	fixedHistoryReadable      = 10809
	fixedHistoryWindowRows    = 7948
	fixedHistoryExcerptRows   = 2861
	fixedHistoryUnique        = 793
	fixedHistoryMaxID         = int64(15528)
	fixedHistorySourceSHA     = "b20980588c83b5a8e1046ce5ed0bc8473c8920fefc431b7b045fdf7b019bd8d6"
	fixedHistoryBaseURL       = "https://api.deepseek.com"
	fixedHistoryModel         = "deepseek-v4-flash"
	fixedHistoryChannelMS     = 3000
	fixedHistoryTotalMS       = 10000
	fixedHistoryWorkers       = 8
	maxHistorySourceLineBytes = 16 * 1024 * 1024
	maxEvaluationAPIKeyBytes  = 16 * 1024
	maxEvaluationWorkers      = 128
)

type historyOptions struct {
	InputPath        string
	ResultsPath      string
	SummaryPath      string
	BaseURL          string
	Model            string
	ChannelTimeout   int
	TotalTimeout     int
	Workers          int
	APIKeyFD         int
	ExpectedSHA      string
	ExpectedEligible int
	ExpectedReadable int
	ExpectedWindows  int
	ExpectedExcerpts int
	ExpectedUnique   int
	ExpectedMaxID    int64
	ValidateOnly     bool
	Fixture          bool
}

type historySourceRecord struct {
	ID              int64                   `json:"id"`
	CreatedAt       string                  `json:"created_at"`
	InputHash       string                  `json:"input_hash"`
	FragmentRole    string                  `json:"fragment_role"`
	FragmentKind    string                  `json:"fragment_kind"`
	ContextClass    string                  `json:"context_class"`
	KeywordTier     string                  `json:"keyword_tier"`
	Action          string                  `json:"action"`
	Flagged         bool                    `json:"flagged"`
	OldCategory     string                  `json:"old_category"`
	DecisionSource  string                  `json:"decision_source"`
	ModelProfile    string                  `json:"model_profile"`
	PolicyVersion   string                  `json:"policy_version"`
	Protocol        string                  `json:"protocol"`
	Provider        string                  `json:"provider"`
	Model           string                  `json:"model"`
	InputExcerpt    string                  `json:"input_excerpt"`
	EvidenceWindows []historyEvidenceWindow `json:"evidence_windows"`
}

type historyEvidenceWindow struct {
	Text string `json:"text"`
}

type preparedHistoryRecord struct {
	Source       historySourceRecord
	Evidence     string
	EvidenceMode string
	EvidenceHash string
	DedupeHash   string
}

type historyGroup struct {
	DedupeHash       string
	EvidenceHash     string
	Evidence         string
	ContextClass     string
	Role             string
	Kind             string
	RepresentativeID int64
	RecordIndexes    []int
}

type historyPreparation struct {
	Records      []preparedHistoryRecord
	Groups       []historyGroup
	SourceSHA256 string
	MinID        int64
	MaxID        int64
	Readable     int
	Unreadable   int
	WindowRows   int
	ExcerptRows  int
}

type groupEvaluation struct {
	Result service.ContentModerationDeepSeekEvaluationResult
	Error  error
}

type historyRecordResult struct {
	Schema             string                                   `json:"schema"`
	ID                 int64                                    `json:"id"`
	CreatedAt          string                                   `json:"created_at,omitempty"`
	InputHash          string                                   `json:"input_hash,omitempty"`
	EvidenceHash       string                                   `json:"evidence_hash,omitempty"`
	EvidenceMode       string                                   `json:"evidence_mode,omitempty"`
	DedupeKeyHash      string                                   `json:"dedupe_key_hash,omitempty"`
	DedupeGroupSize    int                                      `json:"dedupe_group_size,omitempty"`
	RepresentativeID   int64                                    `json:"representative_id,omitempty"`
	Status             string                                   `json:"status"`
	ErrorCode          string                                   `json:"error_code,omitempty"`
	ContextClass       string                                   `json:"context_class,omitempty"`
	FragmentRole       string                                   `json:"fragment_role,omitempty"`
	FragmentKind       string                                   `json:"fragment_kind,omitempty"`
	KeywordTier        string                                   `json:"keyword_tier,omitempty"`
	HistoricalFlagged  bool                                     `json:"historical_flagged"`
	HistoricalCategory string                                   `json:"historical_category,omitempty"`
	HistoricalAction   string                                   `json:"historical_action,omitempty"`
	HistoricalSource   string                                   `json:"historical_source,omitempty"`
	TargetPolicy       string                                   `json:"target_policy_version"`
	DeepSeekFlagged    *bool                                    `json:"deepseek_flagged,omitempty"`
	DeepSeekConfidence *float64                                 `json:"deepseek_confidence,omitempty"`
	DeepSeekCategory   string                                   `json:"deepseek_category,omitempty"`
	DeepSeekReasonHash string                                   `json:"deepseek_reason_hash,omitempty"`
	LatencyMS          int                                      `json:"latency_ms,omitempty"`
	Disagreement       bool                                     `json:"disagreement,omitempty"`
	Attempts           []service.ContentModerationReviewAttempt `json:"attempts,omitempty"`
}

type historyLatencySummary struct {
	P50 int `json:"p50"`
	P95 int `json:"p95"`
	P99 int `json:"p99"`
	Max int `json:"max"`
}

type historyPerformanceGate struct {
	Required       bool    `json:"required"`
	Passed         bool    `json:"passed"`
	MinimumCalls   int     `json:"minimum_calls"`
	MinimumSuccess float64 `json:"minimum_success_rate"`
	MaximumP95MS   int     `json:"maximum_p95_ms"`
	MaximumP99MS   int     `json:"maximum_p99_ms"`
	ActualSuccess  float64 `json:"actual_success_rate"`
}

type historyModelConfig struct {
	BaseURL          string `json:"base_url"`
	Model            string `json:"model"`
	Thinking         string `json:"thinking"`
	ChannelTimeout   int    `json:"channel_timeout_ms"`
	TotalTimeout     int    `json:"total_timeout_ms"`
	Workers          int    `json:"workers"`
	CredentialSource string `json:"credential_source"`
}

type historySummary struct {
	Schema                  string                 `json:"schema"`
	Status                  string                 `json:"status"`
	StartedAt               time.Time              `json:"started_at"`
	FinishedAt              time.Time              `json:"finished_at"`
	Commit                  string                 `json:"commit"`
	VCSModified             bool                   `json:"vcs_modified"`
	SourceSHA256            string                 `json:"source_sha256"`
	ExpectedSourceSHA256    string                 `json:"expected_source_sha256"`
	FixedSnapshot           bool                   `json:"fixed_snapshot"`
	FixedMaxID              int64                  `json:"fixed_max_id"`
	LaterRecordsExcluded    bool                   `json:"later_records_excluded"`
	TargetPolicyVersion     string                 `json:"target_policy_version"`
	DeduplicationKeyFields  []string               `json:"deduplication_key_fields"`
	Eligible                int                    `json:"eligible"`
	Readable                int                    `json:"readable"`
	Unreadable              int                    `json:"unreadable"`
	EvidenceWindowRecords   int                    `json:"evidence_window_records"`
	LegacyExcerptRecords    int                    `json:"legacy_redacted_excerpt_records"`
	Processed               int                    `json:"processed"`
	SuccessfulRecords       int                    `json:"successful_records"`
	FailedRecords           int                    `json:"failed_records"`
	UniqueEvidence          int                    `json:"unique_evidence"`
	DuplicateRecords        int                    `json:"duplicate_records"`
	SuccessfulUnique        int                    `json:"successful_unique"`
	FailedUnique            int                    `json:"failed_unique"`
	ModelCalls              int                    `json:"model_calls"`
	DeepSeekFlaggedRecords  int                    `json:"deepseek_flagged_records"`
	DeepSeekSafeRecords     int                    `json:"deepseek_safe_records"`
	HistoricalDisagreements int                    `json:"historical_disagreements"`
	SchemaValid             bool                   `json:"schema_valid"`
	ReasoningFree           bool                   `json:"reasoning_free"`
	LatencyMS               historyLatencySummary  `json:"latency_ms"`
	PerformanceGate         historyPerformanceGate `json:"performance_gate"`
	Categories              map[string]int         `json:"categories"`
	FailureCodes            map[string]int         `json:"failure_codes"`
	ModelConfig             historyModelConfig     `json:"model_config"`
}

func main() {
	options, err := parseHistoryOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "deepseek moderation evaluation configuration failed: %v\n", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if options.ValidateOnly {
		preparation, err := prepareHistory(ctx, options)
		if err != nil {
			fmt.Fprintf(os.Stderr, "deepseek moderation source validation failed: %v\n", err)
			os.Exit(1)
		}
		validation := map[string]any{
			"status": "validated", "source_sha256": preparation.SourceSHA256,
			"eligible": len(preparation.Records), "readable": preparation.Readable,
			"unreadable": preparation.Unreadable, "unique_evidence": len(preparation.Groups),
			"evidence_window_records":         preparation.WindowRows,
			"legacy_redacted_excerpt_records": preparation.ExcerptRows,
			"duplicate_records":               preparation.Readable - len(preparation.Groups),
			"fixed_max_id":                    preparation.MaxID, "later_records_excluded": true,
			"target_policy_version": service.ContentModerationDeepSeekPromptVersion,
		}
		if err := ctx.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "deepseek moderation source validation canceled: %v\n", err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(validation); err != nil {
			fmt.Fprintf(os.Stderr, "write validation summary: %v\n", err)
			os.Exit(1)
		}
		return
	}
	key, err := readEvaluationAPIKey(ctx, options.APIKeyFD)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read protected DeepSeek credential: %v\n", err)
		os.Exit(1)
	}
	defer zeroBytes(key)
	if err := runHistory(ctx, options, string(key)); err != nil {
		fmt.Fprintf(os.Stderr, "deepseek moderation evaluation failed: %v\n", err)
		os.Exit(1)
	}
}

func parseHistoryOptions(args []string) (historyOptions, error) {
	options := historyOptions{}
	flags := flag.NewFlagSet("deepseek-moderation-eval", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.InputPath, "input", "", "fixed history source NDJSON")
	flags.StringVar(&options.ResultsPath, "results", "", "new per-record result NDJSON")
	flags.StringVar(&options.SummaryPath, "summary", "", "new machine summary JSON")
	options.BaseURL = fixedHistoryBaseURL
	options.Model = fixedHistoryModel
	options.ChannelTimeout = fixedHistoryChannelMS
	options.TotalTimeout = fixedHistoryTotalMS
	options.Workers = fixedHistoryWorkers
	flags.IntVar(&options.APIKeyFD, "api-key-fd", 3, "protected file descriptor containing the API key")
	options.ExpectedSHA = fixedHistorySourceSHA
	options.ExpectedEligible = fixedHistoryEligible
	options.ExpectedReadable = fixedHistoryReadable
	options.ExpectedWindows = fixedHistoryWindowRows
	options.ExpectedExcerpts = fixedHistoryExcerptRows
	options.ExpectedUnique = fixedHistoryUnique
	options.ExpectedMaxID = fixedHistoryMaxID
	flags.BoolVar(&options.ValidateOnly, "validate-only", false, "validate fixed source without reading a credential or calling the model")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, errors.New("positional arguments are not accepted")
	}
	if strings.TrimSpace(options.InputPath) == "" {
		return options, errors.New("--input is required")
	}
	if options.ExpectedEligible < 1 || options.ExpectedReadable < 1 || options.ExpectedReadable > options.ExpectedEligible {
		return options, errors.New("expected record counts are invalid")
	}
	if options.ExpectedWindows < 0 || options.ExpectedExcerpts < 0 ||
		options.ExpectedWindows+options.ExpectedExcerpts != options.ExpectedReadable {
		return options, errors.New("expected evidence source counts are invalid")
	}
	if options.ExpectedUnique < 1 || options.ExpectedUnique > options.ExpectedReadable {
		return options, errors.New("expected unique evidence count is invalid")
	}
	if options.ExpectedMaxID < 1 {
		return options, errors.New("expected maximum ID is invalid")
	}
	if len(strings.TrimSpace(options.ExpectedSHA)) != sha256.Size*2 {
		return options, errors.New("expected source SHA-256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(strings.TrimSpace(options.ExpectedSHA)); err != nil {
		return options, errors.New("expected source SHA-256 is invalid")
	}
	if options.ValidateOnly {
		return options, nil
	}
	if strings.TrimSpace(options.ResultsPath) == "" || strings.TrimSpace(options.SummaryPath) == "" {
		return options, errors.New("--results and --summary are required")
	}
	if options.Workers < 1 || options.Workers > maxEvaluationWorkers {
		return options, fmt.Errorf("workers must be between 1 and %d", maxEvaluationWorkers)
	}
	if options.APIKeyFD < 3 {
		return options, errors.New("API key must be supplied through file descriptor 3 or higher")
	}
	if options.ChannelTimeout < 100 || options.ChannelTimeout > 30000 {
		return options, errors.New("channel timeout must be between 100 and 30000 milliseconds")
	}
	if options.TotalTimeout < 100 || options.TotalTimeout > 120000 {
		return options, errors.New("total timeout must be between 100 and 120000 milliseconds")
	}
	return options, nil
}

func readEvaluationAPIKey(ctx context.Context, fdNumber int) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	file := os.NewFile(uintptr(fdNumber), "deepseek-api-key")
	if file == nil {
		return nil, errors.New("credential file descriptor is unavailable")
	}
	defer func() { _ = file.Close() }()
	type readResult struct {
		value []byte
		err   error
	}
	result := make(chan readResult, 1)
	go func() {
		value, readErr := io.ReadAll(io.LimitReader(file, maxEvaluationAPIKeyBytes+1))
		result <- readResult{value: value, err: readErr}
	}()
	var read readResult
	select {
	case read = <-result:
		if ctxErr := ctx.Err(); ctxErr != nil {
			zeroBytes(read.value)
			return nil, ctxErr
		}
	case <-ctx.Done():
		_ = file.Close()
		read = <-result
		zeroBytes(read.value)
		return nil, ctx.Err()
	}
	if read.err != nil {
		zeroBytes(read.value)
		return nil, errors.New("credential file descriptor could not be read")
	}
	value := read.value
	if len(value) > maxEvaluationAPIKeyBytes {
		zeroBytes(value)
		return nil, errors.New("credential exceeds the maximum length")
	}
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		zeroBytes(value)
		return nil, errors.New("credential is empty")
	}
	key := append([]byte(nil), trimmed...)
	zeroBytes(value)
	return key, nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func prepareHistory(ctx context.Context, options historyOptions) (historyPreparation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validatePrivateDirectory(filepath.Dir(options.InputPath)); err != nil {
		return historyPreparation{}, fmt.Errorf("fixed source parent: %w", err)
	}
	info, err := os.Lstat(options.InputPath)
	if err != nil {
		return historyPreparation{}, fmt.Errorf("inspect fixed source: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return historyPreparation{}, errors.New("fixed source must be a private regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return historyPreparation{}, errors.New("fixed source must be owned by the evaluator user")
	}
	file, err := os.Open(options.InputPath)
	if err != nil {
		return historyPreparation{}, fmt.Errorf("open fixed source: %w", err)
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(file, hasher))
	scanner.Buffer(make([]byte, 64*1024), maxHistorySourceLineBytes)
	preparation := historyPreparation{MinID: math.MaxInt64}
	seenIDs := make(map[int64]struct{}, options.ExpectedEligible)
	groupsByHash := make(map[string]int, options.ExpectedUnique)
	for line := 1; ; line++ {
		select {
		case <-ctx.Done():
			return historyPreparation{}, ctx.Err()
		default:
		}
		if !scanner.Scan() {
			break
		}
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			return historyPreparation{}, fmt.Errorf("source line %d is empty", line)
		}
		var source historySourceRecord
		if err := json.Unmarshal(scanner.Bytes(), &source); err != nil {
			return historyPreparation{}, fmt.Errorf("source line %d is invalid JSON", line)
		}
		if source.ID < 1 {
			return historyPreparation{}, fmt.Errorf("source line %d has an invalid ID", line)
		}
		if _, exists := seenIDs[source.ID]; exists {
			return historyPreparation{}, fmt.Errorf("source contains duplicate ID %d", source.ID)
		}
		seenIDs[source.ID] = struct{}{}
		if source.ID < preparation.MinID {
			preparation.MinID = source.ID
		}
		if source.ID > preparation.MaxID {
			preparation.MaxID = source.ID
		}
		evidence, evidenceMode := readableHistoryEvidenceWithMode(source)
		record := preparedHistoryRecord{Source: source, Evidence: evidence, EvidenceMode: evidenceMode}
		if evidence == "" {
			preparation.Unreadable++
			preparation.Records = append(preparation.Records, record)
			continue
		}
		preparation.Readable++
		switch evidenceMode {
		case "evidence_windows":
			preparation.WindowRows++
		case "legacy_redacted_excerpt":
			preparation.ExcerptRows++
		default:
			return historyPreparation{}, errors.New("readable history evidence has an unknown mode")
		}
		record.EvidenceHash = sha256String(evidence)
		contextClass := normalizeHistoryContext(source.ContextClass)
		role := strings.ToLower(strings.TrimSpace(source.FragmentRole))
		kind := strings.ToLower(strings.TrimSpace(source.FragmentKind))
		canonical, err := json.Marshal([]string{
			service.ContentModerationDeepSeekPromptVersion, contextClass, role, kind, evidence,
		})
		if err != nil {
			return historyPreparation{}, errors.New("build deduplication key")
		}
		record.DedupeHash = sha256String(string(canonical))
		recordIndex := len(preparation.Records)
		if groupIndex, exists := groupsByHash[record.DedupeHash]; exists {
			group := &preparation.Groups[groupIndex]
			if group.Evidence != evidence || group.ContextClass != contextClass || group.Role != role || group.Kind != kind {
				return historyPreparation{}, errors.New("deduplication hash collision")
			}
			group.RecordIndexes = append(group.RecordIndexes, recordIndex)
		} else {
			groupsByHash[record.DedupeHash] = len(preparation.Groups)
			preparation.Groups = append(preparation.Groups, historyGroup{
				DedupeHash: record.DedupeHash, EvidenceHash: record.EvidenceHash, Evidence: evidence,
				ContextClass: contextClass, Role: role, Kind: kind,
				RepresentativeID: source.ID, RecordIndexes: []int{recordIndex},
			})
		}
		preparation.Records = append(preparation.Records, record)
	}
	if err := ctx.Err(); err != nil {
		return historyPreparation{}, err
	}
	if err := scanner.Err(); err != nil {
		return historyPreparation{}, fmt.Errorf("read fixed source: %w", err)
	}
	preparation.SourceSHA256 = hex.EncodeToString(hasher.Sum(nil))
	if len(preparation.Records) == 0 {
		preparation.MinID = 0
	}
	sort.SliceStable(preparation.Groups, func(i, j int) bool {
		return preparation.Groups[i].RepresentativeID < preparation.Groups[j].RepresentativeID
	})
	if err := ctx.Err(); err != nil {
		return historyPreparation{}, err
	}
	if err := validateHistoryPreparation(preparation, options); err != nil {
		return preparation, err
	}
	return preparation, nil
}

func validateHistoryPreparation(preparation historyPreparation, options historyOptions) error {
	if !strings.EqualFold(preparation.SourceSHA256, strings.TrimSpace(options.ExpectedSHA)) {
		return errors.New("fixed source SHA-256 does not match the pinned snapshot")
	}
	if len(preparation.Records) != options.ExpectedEligible {
		return fmt.Errorf("fixed source has %d records, expected %d", len(preparation.Records), options.ExpectedEligible)
	}
	if preparation.Readable != options.ExpectedReadable {
		return fmt.Errorf("fixed source has %d readable records, expected %d", preparation.Readable, options.ExpectedReadable)
	}
	if preparation.Unreadable != options.ExpectedEligible-options.ExpectedReadable {
		return errors.New("eligible count does not equal readable plus unreadable")
	}
	if preparation.WindowRows != options.ExpectedWindows {
		return fmt.Errorf("fixed source has %d evidence-window records, expected %d", preparation.WindowRows, options.ExpectedWindows)
	}
	if preparation.ExcerptRows != options.ExpectedExcerpts {
		return fmt.Errorf("fixed source has %d legacy redacted excerpt records, expected %d", preparation.ExcerptRows, options.ExpectedExcerpts)
	}
	if preparation.WindowRows+preparation.ExcerptRows != preparation.Readable {
		return errors.New("readable count does not equal evidence-window plus legacy excerpt records")
	}
	if len(preparation.Groups) != options.ExpectedUnique {
		return fmt.Errorf("fixed source has %d unique evidence groups, expected %d", len(preparation.Groups), options.ExpectedUnique)
	}
	if preparation.MaxID != options.ExpectedMaxID {
		return fmt.Errorf("fixed source maximum ID is %d, expected %d", preparation.MaxID, options.ExpectedMaxID)
	}
	return nil
}

func readableHistoryEvidence(source historySourceRecord) string {
	evidence, _ := readableHistoryEvidenceWithMode(source)
	return evidence
}

func readableHistoryEvidenceWithMode(source historySourceRecord) (string, string) {
	parts := make([]string, 0, len(source.EvidenceWindows))
	for _, window := range source.EvidenceWindows {
		text := strings.TrimSpace(window.Text)
		if text == "" {
			continue
		}
		parts = append(parts, text)
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n"), "evidence_windows"
	}
	if excerpt := strings.TrimSpace(source.InputExcerpt); excerpt != "" {
		return excerpt, "legacy_redacted_excerpt"
	}
	return "", ""
}

func normalizeHistoryContext(value string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case "user", "tool", "assistant_untrusted", "service_log", "code", "config":
		return normalized
	default:
		return "unknown"
	}
}

func sha256String(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func normalizeHistoryOptions(options historyOptions) historyOptions {
	options.BaseURL = strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if options.BaseURL == "" {
		options.BaseURL = fixedHistoryBaseURL
	}
	options.Model = strings.TrimSpace(options.Model)
	if options.Model == "" {
		options.Model = fixedHistoryModel
	}
	if options.ChannelTimeout <= 0 {
		options.ChannelTimeout = fixedHistoryChannelMS
	}
	if options.ChannelTimeout > 30000 {
		options.ChannelTimeout = 30000
	}
	if options.TotalTimeout <= 0 {
		options.TotalTimeout = fixedHistoryTotalMS
	}
	if options.Workers <= 0 {
		options.Workers = fixedHistoryWorkers
	}
	if options.Workers > maxEvaluationWorkers {
		options.Workers = maxEvaluationWorkers
	}
	return options
}

func matchesFixedHistoryContract(options historyOptions) bool {
	return options.BaseURL == fixedHistoryBaseURL &&
		options.Model == fixedHistoryModel &&
		options.ChannelTimeout == fixedHistoryChannelMS &&
		options.TotalTimeout == fixedHistoryTotalMS &&
		options.Workers == fixedHistoryWorkers &&
		strings.EqualFold(strings.TrimSpace(options.ExpectedSHA), fixedHistorySourceSHA) &&
		options.ExpectedEligible == fixedHistoryEligible &&
		options.ExpectedReadable == fixedHistoryReadable &&
		options.ExpectedWindows == fixedHistoryWindowRows &&
		options.ExpectedExcerpts == fixedHistoryExcerptRows &&
		options.ExpectedUnique == fixedHistoryUnique &&
		options.ExpectedMaxID == fixedHistoryMaxID
}

func runHistory(ctx context.Context, options historyOptions, apiKey string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	options = normalizeHistoryOptions(options)
	if !options.Fixture && !matchesFixedHistoryContract(options) {
		return errors.New("evaluation options do not match the immutable production history contract")
	}
	if err := preflightHistoryOutputs(options); err != nil {
		return err
	}
	started := time.Now().UTC()
	preparation, err := prepareHistory(ctx, options)
	if err != nil {
		return err
	}
	evaluator, err := service.NewContentModerationDeepSeekEvaluator([]service.ContentModerationDeepSeekChannel{{
		ID: "history-official", Name: "DeepSeek official history evaluation", BaseURL: options.BaseURL,
		Model: options.Model, Enabled: true, Order: 0, TimeoutMS: options.ChannelTimeout, APIKey: apiKey,
	}}, options.TotalTimeout)
	if err != nil {
		return fmt.Errorf("configure evaluator: %w", err)
	}
	evaluations := evaluateHistoryGroups(ctx, evaluator, preparation.Groups, options.Workers)
	if err := ctx.Err(); err != nil {
		return err
	}
	records, summary := buildHistoryOutputs(options, preparation, evaluations, started)
	if err := writeNDJSONAtomic(ctx, options.ResultsPath, records); err != nil {
		return fmt.Errorf("write result records: %w", err)
	}
	if err := writeJSONAtomic(ctx, options.SummaryPath, summary); err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	if summary.Status != "passed" {
		return fmt.Errorf("evaluation report failed with %d unique model failures", summary.FailedUnique)
	}
	return nil
}

func evaluateHistoryGroups(
	ctx context.Context,
	evaluator *service.ContentModerationDeepSeekEvaluator,
	groups []historyGroup,
	workers int,
) []groupEvaluation {
	results := make([]groupEvaluation, len(groups))
	jobs := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				group := groups[index]
				result, err := evaluator.Evaluate(ctx, service.ContentModerationDeepSeekEvaluationInput{
					Text: group.Evidence, ContextClass: group.ContextClass, Role: group.Role, Kind: group.Kind,
				})
				results[index] = groupEvaluation{Result: result, Error: err}
			}
		}()
	}
	for index := range groups {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			wait.Wait()
			for remaining := index; remaining < len(groups); remaining++ {
				if results[remaining].Error == nil && len(results[remaining].Result.Attempts) == 0 {
					results[remaining].Error = ctx.Err()
				}
			}
			return results
		}
	}
	close(jobs)
	wait.Wait()
	return results
}

func buildHistoryOutputs(
	options historyOptions,
	preparation historyPreparation,
	evaluations []groupEvaluation,
	started time.Time,
) ([]historyRecordResult, historySummary) {
	commit, modified := buildVersion()
	summary := historySummary{
		Schema: historySchema, Status: "passed", StartedAt: started, FinishedAt: time.Now().UTC(),
		Commit: commit, VCSModified: modified, SourceSHA256: preparation.SourceSHA256,
		ExpectedSourceSHA256: strings.ToLower(strings.TrimSpace(options.ExpectedSHA)),
		FixedSnapshot:        !options.Fixture, FixedMaxID: preparation.MaxID, LaterRecordsExcluded: !options.Fixture,
		TargetPolicyVersion:    service.ContentModerationDeepSeekPromptVersion,
		DeduplicationKeyFields: []string{"target_policy_version", "context_class", "fragment_role", "fragment_kind", "evidence"},
		Eligible:               len(preparation.Records), Readable: preparation.Readable, Unreadable: preparation.Unreadable,
		EvidenceWindowRecords: preparation.WindowRows, LegacyExcerptRecords: preparation.ExcerptRows,
		UniqueEvidence: len(preparation.Groups), DuplicateRecords: preparation.Readable - len(preparation.Groups),
		SchemaValid: true, ReasoningFree: true, Categories: make(map[string]int), FailureCodes: make(map[string]int),
		ModelConfig: historyModelConfig{
			BaseURL: strings.TrimRight(strings.TrimSpace(options.BaseURL), "/"), Model: strings.TrimSpace(options.Model),
			Thinking: "disabled", ChannelTimeout: options.ChannelTimeout, TotalTimeout: options.TotalTimeout,
			Workers: options.Workers, CredentialSource: "protected_file_descriptor",
		},
	}
	outputs := make([]historyRecordResult, len(preparation.Records))
	for index, record := range preparation.Records {
		outputs[index] = historyRecordResult{
			Schema: historySchema, ID: record.Source.ID, CreatedAt: record.Source.CreatedAt,
			InputHash: record.Source.InputHash, EvidenceHash: record.EvidenceHash, EvidenceMode: record.EvidenceMode,
			DedupeKeyHash: record.DedupeHash,
			Status:        "unreadable", ErrorCode: "no_readable_evidence",
			ContextClass: normalizeHistoryContext(record.Source.ContextClass),
			FragmentRole: strings.ToLower(strings.TrimSpace(record.Source.FragmentRole)),
			FragmentKind: strings.ToLower(strings.TrimSpace(record.Source.FragmentKind)),
			KeywordTier:  record.Source.KeywordTier, HistoricalFlagged: record.Source.Flagged,
			HistoricalCategory: record.Source.OldCategory, HistoricalAction: record.Source.Action,
			HistoricalSource: record.Source.DecisionSource, TargetPolicy: service.ContentModerationDeepSeekPromptVersion,
		}
	}
	latencies := make([]int, 0, len(preparation.Groups))
	for groupIndex, group := range preparation.Groups {
		evaluation := evaluations[groupIndex]
		code := evaluationErrorCode(evaluation)
		if evaluation.Error != nil {
			summary.Status = "failed"
			summary.SchemaValid = false
			summary.ReasoningFree = false
			summary.FailedUnique++
			summary.FailureCodes[code]++
		} else {
			summary.SuccessfulUnique++
			latencies = append(latencies, evaluation.Result.LatencyMS)
		}
		for _, attempt := range evaluation.Result.Attempts {
			if attempt.Outcome != "skipped" {
				summary.ModelCalls++
			}
		}
		for _, recordIndex := range group.RecordIndexes {
			output := &outputs[recordIndex]
			output.DedupeGroupSize = len(group.RecordIndexes)
			output.RepresentativeID = group.RepresentativeID
			output.Attempts = append([]service.ContentModerationReviewAttempt(nil), evaluation.Result.Attempts...)
			if evaluation.Error != nil {
				output.Status = "failed"
				output.ErrorCode = code
				summary.FailedRecords++
				continue
			}
			flagged := evaluation.Result.Flagged
			confidence := evaluation.Result.Confidence
			output.Status = "processed"
			output.ErrorCode = ""
			output.DeepSeekFlagged = &flagged
			output.DeepSeekConfidence = &confidence
			output.DeepSeekCategory = evaluation.Result.Category
			output.DeepSeekReasonHash = evaluation.Result.ReasonHash
			output.LatencyMS = evaluation.Result.LatencyMS
			output.Disagreement = output.HistoricalFlagged != flagged
			summary.Processed++
			summary.SuccessfulRecords++
			summary.Categories[evaluation.Result.Category]++
			if flagged {
				summary.DeepSeekFlaggedRecords++
			} else {
				summary.DeepSeekSafeRecords++
			}
			if output.Disagreement {
				summary.HistoricalDisagreements++
			}
		}
	}
	summary.LatencyMS = summarizeLatencies(latencies)
	summary.PerformanceGate = historyPerformanceGate{
		Required: options.ExpectedUnique >= 100, MinimumCalls: 100, MinimumSuccess: 0.99,
		MaximumP95MS: 2500, MaximumP99MS: 3000,
	}
	if summary.UniqueEvidence > 0 {
		summary.PerformanceGate.ActualSuccess = float64(summary.SuccessfulUnique) / float64(summary.UniqueEvidence)
	}
	summary.PerformanceGate.Passed = summary.ModelCalls >= summary.PerformanceGate.MinimumCalls &&
		summary.PerformanceGate.ActualSuccess >= summary.PerformanceGate.MinimumSuccess &&
		summary.LatencyMS.P95 <= summary.PerformanceGate.MaximumP95MS &&
		summary.LatencyMS.P99 <= summary.PerformanceGate.MaximumP99MS
	summary.FinishedAt = time.Now().UTC()
	if summary.Processed != preparation.Readable || summary.FailedRecords != 0 || summary.FailedUnique != 0 {
		summary.Status = "failed"
	}
	if summary.ModelCalls != summary.UniqueEvidence {
		summary.Status = "failed"
	}
	if summary.PerformanceGate.Required && !summary.PerformanceGate.Passed {
		summary.Status = "failed"
	}
	return outputs, summary
}

func evaluationErrorCode(evaluation groupEvaluation) string {
	if errors.Is(evaluation.Error, context.Canceled) {
		return "canceled"
	}
	if errors.Is(evaluation.Error, context.DeadlineExceeded) {
		return "deadline"
	}
	for index := len(evaluation.Result.Attempts) - 1; index >= 0; index-- {
		if code := strings.TrimSpace(evaluation.Result.Attempts[index].Error); code != "" {
			return code
		}
	}
	return "model_failure"
}

func summarizeLatencies(values []int) historyLatencySummary {
	if len(values) == 0 {
		return historyLatencySummary{}
	}
	values = append([]int(nil), values...)
	sort.Ints(values)
	return historyLatencySummary{
		P50: percentile(values, 0.50), P95: percentile(values, 0.95),
		P99: percentile(values, 0.99), Max: values[len(values)-1],
	}
}

func percentile(sorted []int, value float64) int {
	index := int(math.Ceil(value*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func buildVersion() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", true
	}
	commit := "unknown"
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if strings.TrimSpace(setting.Value) != "" {
				commit = setting.Value
			}
		case "vcs.modified":
			modified, _ = strconv.ParseBool(setting.Value)
		}
	}
	return commit, modified
}

func preflightHistoryOutputs(options historyOptions) error {
	input, err := filepath.Abs(options.InputPath)
	if err != nil {
		return fmt.Errorf("resolve input path: %w", err)
	}
	results, err := filepath.Abs(options.ResultsPath)
	if err != nil {
		return fmt.Errorf("resolve results path: %w", err)
	}
	summary, err := filepath.Abs(options.SummaryPath)
	if err != nil {
		return fmt.Errorf("resolve summary path: %w", err)
	}
	if input == results || input == summary || results == summary {
		return errors.New("input, results, and summary paths must differ")
	}
	for _, path := range []string{results, summary} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite existing output %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect output %s: %w", path, err)
		}
		if err := validatePrivateDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("output parent for %s: %w", path, err)
		}
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("must be an existing non-symlink directory")
	}
	if info.Mode().Perm() != 0o700 {
		return errors.New("must have mode 0700")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("must be owned by the evaluator user")
	}
	return nil
}

func writeNDJSONAtomic(ctx context.Context, path string, records []historyRecordResult) error {
	return writeAtomic(ctx, path, func(writer io.Writer) error {
		encoder := json.NewEncoder(writer)
		encoder.SetEscapeHTML(false)
		for _, record := range records {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := encoder.Encode(record); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeJSONAtomic(ctx context.Context, path string, value historySummary) error {
	return writeAtomic(ctx, path, func(writer io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		encoder := json.NewEncoder(writer)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	})
}

func writeAtomic(ctx context.Context, path string, write func(io.Writer) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := validatePrivateDirectory(filepath.Dir(absPath)); err != nil {
		return fmt.Errorf("output parent: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(absPath), ".deepseek-evaluation-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if err := write(file); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(tempPath, absPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to overwrite existing output %s", absPath)
		}
		return err
	}
	if err := os.Remove(tempPath); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(absPath))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}
