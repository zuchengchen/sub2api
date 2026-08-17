package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRunHistoryDeduplicatesCallsAndMapsEveryRecord(t *testing.T) {
	t.Parallel()
	var mutex sync.Mutex
	requests := make([]map[string]any, 0, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload map[string]any
		require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
		require.Equal(t, "Bearer sk-history-fixture", request.Header.Get("Authorization"))
		mutex.Lock()
		requests = append(requests, payload)
		mutex.Unlock()
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		decision := `{"confidence":0.11,"category":"safe","reason":""}`
		if strings.Contains(string(encoded), "attack other system") {
			decision = `{"confidence":0.93,"category":"cyber_abuse","reason":"attack other system"}`
		}
		writer.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "stop",
				"message":       map[string]any{"content": decision},
			}},
		}))
	}))
	t.Cleanup(server.Close)

	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	sourcePath := filepath.Join(directory, "source.ndjson")
	resultsPath := filepath.Join(directory, "results.ndjson")
	summaryPath := filepath.Join(directory, "summary.json")
	source := strings.Join([]string{
		`{"id":1,"created_at":"2026-08-01T00:00:00Z","input_hash":"hash-1","fragment_role":"user","fragment_kind":"text","context_class":"user","flagged":false,"input_excerpt":"decoy excerpt one","evidence_windows":[{"text":"attack other system"}]}`,
		`{"id":2,"created_at":"2026-08-01T00:01:00Z","input_hash":"hash-2","fragment_role":"user","fragment_kind":"text","context_class":"user","flagged":true,"input_excerpt":"decoy excerpt two","evidence_windows":[{"text":"attack other system"}]}`,
		`{"id":3,"created_at":"2026-08-01T00:02:00Z","input_hash":"hash-3","fragment_role":"tool","fragment_kind":"output","context_class":"tool","flagged":false,"input_excerpt":"decoy ordinary excerpt","evidence_windows":[{"text":"ordinary deployment log"}]}`,
		`{"id":4,"created_at":"2026-08-01T00:03:00Z","input_hash":"hash-4","fragment_role":"user","fragment_kind":"text","context_class":"user","flagged":false,"input_excerpt":"legacy redacted excerpt","evidence_windows":[]}`,
		`{"id":5,"created_at":"2026-08-01T00:04:00Z","input_hash":"hash-5","fragment_role":"user","fragment_kind":"text","context_class":"user","flagged":false,"input_excerpt":"","evidence_windows":[]}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(sourcePath, []byte(source), 0o600))
	digest := sha256.Sum256([]byte(source))
	options := historyOptions{
		InputPath: sourcePath, ResultsPath: resultsPath, SummaryPath: summaryPath,
		BaseURL: server.URL, Model: "", ChannelTimeout: 0,
		TotalTimeout: 0, Workers: 2, ExpectedSHA: hex.EncodeToString(digest[:]),
		ExpectedEligible: 5, ExpectedReadable: 4, ExpectedWindows: 3, ExpectedExcerpts: 1,
		ExpectedUnique: 3, ExpectedMaxID: 5, Fixture: true,
	}
	runErr := runHistory(context.Background(), options, "sk-history-fixture")
	if runErr != nil {
		if value, err := os.ReadFile(summaryPath); err == nil {
			t.Logf("failed summary: %s", value)
		}
		if value, err := os.ReadFile(resultsPath); err == nil {
			t.Logf("failed results: %s", value)
		}
	}
	require.NoError(t, runErr)

	mutex.Lock()
	captured := append([]map[string]any(nil), requests...)
	mutex.Unlock()
	require.Len(t, captured, 3)
	for _, payload := range captured {
		require.Equal(t, map[string]any{"type": "disabled"}, payload["thinking"])
		require.Equal(t, map[string]any{"type": "json_object"}, payload["response_format"])
		require.NotContains(t, payload, "reasoning_effort")
		require.Equal(t, float64(0), payload["temperature"])
		require.Equal(t, float64(64), payload["max_tokens"])
	}
	capturedJSON, err := json.Marshal(captured)
	require.NoError(t, err)
	require.Contains(t, string(capturedJSON), "attack other system")
	require.Contains(t, string(capturedJSON), "ordinary deployment log")
	require.Contains(t, string(capturedJSON), "legacy redacted excerpt")
	require.NotContains(t, string(capturedJSON), "decoy excerpt")

	resultsFile, err := os.Open(resultsPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resultsFile.Close() })
	var results []historyRecordResult
	scanner := bufio.NewScanner(resultsFile)
	for scanner.Scan() {
		var result historyRecordResult
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &result))
		results = append(results, result)
	}
	require.NoError(t, scanner.Err())
	require.Len(t, results, 5)
	require.Equal(t, "processed", results[0].Status)
	require.Equal(t, 2, results[0].DedupeGroupSize)
	require.Equal(t, int64(1), results[0].RepresentativeID)
	require.Equal(t, results[0].DedupeKeyHash, results[1].DedupeKeyHash)
	require.NotNil(t, results[0].DeepSeekFlagged)
	require.True(t, *results[0].DeepSeekFlagged)
	require.NotEmpty(t, results[0].DeepSeekReasonHash)
	require.Equal(t, "processed", results[2].Status)
	require.False(t, *results[2].DeepSeekFlagged)
	require.Equal(t, "processed", results[3].Status)
	require.Equal(t, "legacy_redacted_excerpt", results[3].EvidenceMode)
	require.Equal(t, "unreadable", results[4].Status)
	require.Equal(t, "no_readable_evidence", results[4].ErrorCode)
	require.NotEmpty(t, results[0].EvidenceHash)
	require.NotContains(t, string(mustReadFile(t, resultsPath)), "attack other system")
	require.NotContains(t, string(mustReadFile(t, resultsPath)), "ordinary deployment log")

	var summary historySummary
	require.NoError(t, json.Unmarshal(mustReadFile(t, summaryPath), &summary))
	require.Equal(t, "passed", summary.Status)
	require.Equal(t, 5, summary.Eligible)
	require.Equal(t, 4, summary.Readable)
	require.Equal(t, 1, summary.Unreadable)
	require.Equal(t, 3, summary.EvidenceWindowRecords)
	require.Equal(t, 1, summary.LegacyExcerptRecords)
	require.Equal(t, 4, summary.Processed)
	require.Equal(t, 3, summary.UniqueEvidence)
	require.Equal(t, 1, summary.DuplicateRecords)
	require.Equal(t, 3, summary.ModelCalls)
	require.True(t, summary.SchemaValid)
	require.True(t, summary.ReasoningFree)
	require.Equal(t, "deepseek-v4-flash", summary.ModelConfig.Model)
	require.Equal(t, 3000, summary.ModelConfig.ChannelTimeout)
	require.Equal(t, 10000, summary.ModelConfig.TotalTimeout)
	require.Equal(t, []string{"target_policy_version", "context_class", "fragment_role", "fragment_kind", "evidence"}, summary.DeduplicationKeyFields)

	resultsInfo, err := os.Stat(resultsPath)
	require.NoError(t, err)
	summaryInfo, err := os.Stat(summaryPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), resultsInfo.Mode().Perm())
	require.Equal(t, os.FileMode(0o600), summaryInfo.Mode().Perm())
}

func TestRunHistoryRefusesExistingOutputsBeforeCallingModel(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	resultsPath := filepath.Join(directory, "results.ndjson")
	require.NoError(t, os.WriteFile(resultsPath, []byte("owned\n"), 0o600))
	options := historyOptions{
		InputPath: filepath.Join(directory, "missing-source.ndjson"), ResultsPath: resultsPath,
		SummaryPath: filepath.Join(directory, "summary.json"), Fixture: true,
	}
	err := runHistory(context.Background(), options, "fixture")
	require.ErrorContains(t, err, "refusing to overwrite")
	require.Equal(t, "owned\n", string(mustReadFile(t, resultsPath)))
}

func TestParseHistoryOptionsPinsSnapshotAndOfficialModel(t *testing.T) {
	t.Parallel()
	options, err := parseHistoryOptions([]string{"--input", "/private/source.ndjson", "--validate-only"})
	require.NoError(t, err)
	require.Equal(t, fixedHistoryBaseURL, options.BaseURL)
	require.Equal(t, fixedHistoryModel, options.Model)
	require.Equal(t, fixedHistorySourceSHA, options.ExpectedSHA)
	require.Equal(t, fixedHistoryEligible, options.ExpectedEligible)
	require.Equal(t, fixedHistoryReadable, options.ExpectedReadable)
	require.Equal(t, fixedHistoryWindowRows, options.ExpectedWindows)
	require.Equal(t, fixedHistoryExcerptRows, options.ExpectedExcerpts)
	require.Equal(t, options.ExpectedReadable, options.ExpectedWindows+options.ExpectedExcerpts)
	require.Equal(t, 70, options.ExpectedEligible-options.ExpectedReadable)
	require.Equal(t, fixedHistoryUnique, options.ExpectedUnique)
	require.Equal(t, fixedHistoryMaxID, options.ExpectedMaxID)
	require.Equal(t, fixedHistoryChannelMS, options.ChannelTimeout)
	require.Equal(t, fixedHistoryTotalMS, options.TotalTimeout)
	require.Equal(t, fixedHistoryWorkers, options.Workers)
	require.Equal(t, service.DefaultContentModerationDeepSeekBaseURL, fixedHistoryBaseURL)
	require.Equal(t, service.DefaultContentModerationDeepSeekModel, fixedHistoryModel)
	require.True(t, matchesFixedHistoryContract(options))

	changed := options
	changed.ChannelTimeout++
	require.False(t, matchesFixedHistoryContract(changed))
	changed = options
	changed.TotalTimeout++
	require.False(t, matchesFixedHistoryContract(changed))
	changed = options
	changed.Workers++
	require.False(t, matchesFixedHistoryContract(changed))
	changed = options
	changed.ExpectedWindows--
	changed.ExpectedExcerpts++
	require.False(t, matchesFixedHistoryContract(changed))

	_, err = parseHistoryOptions([]string{"--input", "/private/source.ndjson", "--validate-only", "--base-url", "https://example.invalid"})
	require.Error(t, err)
	_, err = parseHistoryOptions([]string{"--input", "/private/source.ndjson", "--validate-only", "--expected-unique", "1"})
	require.Error(t, err)
	_, err = parseHistoryOptions([]string{"--input", "/private/source.ndjson", "--validate-only", "--model", "other-model"})
	require.Error(t, err)
	_, err = parseHistoryOptions([]string{"--input", "/private/source.ndjson", "--validate-only", "--api-key", "not-allowed"})
	require.Error(t, err)
	_, err = parseHistoryOptions([]string{"--input", "/private/source.ndjson", "--validate-only", "--channel-timeout-ms", "100"})
	require.Error(t, err)
	_, err = parseHistoryOptions([]string{"--input", "/private/source.ndjson", "--validate-only", "--workers", "1"})
	require.Error(t, err)
}

func TestNormalizeHistoryOptionsRecordsEffectiveConfiguration(t *testing.T) {
	t.Parallel()
	normalized := normalizeHistoryOptions(historyOptions{
		BaseURL: "  ", Model: "  ", ChannelTimeout: -1, TotalTimeout: 0, Workers: 0,
	})
	require.Equal(t, fixedHistoryBaseURL, normalized.BaseURL)
	require.Equal(t, fixedHistoryModel, normalized.Model)
	require.Equal(t, fixedHistoryChannelMS, normalized.ChannelTimeout)
	require.Equal(t, fixedHistoryTotalMS, normalized.TotalTimeout)
	require.Equal(t, fixedHistoryWorkers, normalized.Workers)

	normalized = normalizeHistoryOptions(historyOptions{ChannelTimeout: 999999, Workers: 999999})
	require.Equal(t, 30000, normalized.ChannelTimeout)
	require.Equal(t, maxEvaluationWorkers, normalized.Workers)
}

func TestValidateHistoryPreparationPinsCompleteSnapshotContract(t *testing.T) {
	t.Parallel()
	options, err := parseHistoryOptions([]string{"--input", "/private/source.ndjson", "--validate-only"})
	require.NoError(t, err)
	preparation := historyPreparation{
		Records:      make([]preparedHistoryRecord, fixedHistoryEligible),
		Groups:       make([]historyGroup, fixedHistoryUnique),
		SourceSHA256: fixedHistorySourceSHA,
		MaxID:        fixedHistoryMaxID,
		Readable:     fixedHistoryReadable,
		Unreadable:   fixedHistoryEligible - fixedHistoryReadable,
		WindowRows:   fixedHistoryWindowRows,
		ExcerptRows:  fixedHistoryExcerptRows,
	}
	require.NoError(t, validateHistoryPreparation(preparation, options))

	changed := preparation
	changed.Readable--
	require.Error(t, validateHistoryPreparation(changed, options))
	changed = preparation
	changed.Unreadable--
	require.Error(t, validateHistoryPreparation(changed, options))
	changed = preparation
	changed.WindowRows--
	changed.ExcerptRows++
	require.Error(t, validateHistoryPreparation(changed, options))
	changed = preparation
	changed.Groups = changed.Groups[:len(changed.Groups)-1]
	require.Error(t, validateHistoryPreparation(changed, options))
	changed = preparation
	changed.MaxID--
	require.Error(t, validateHistoryPreparation(changed, options))
	changed = preparation
	changed.SourceSHA256 = strings.Repeat("0", sha256.Size*2)
	require.Error(t, validateHistoryPreparation(changed, options))
}

func TestReadEvaluationAPIKeyUsesProtectedDescriptor(t *testing.T) {
	t.Parallel()
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	_, err = writer.WriteString("  sk-fd-fixture  \n")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	ownedFD, err := syscall.Dup(int(reader.Fd()))
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	value, err := readEvaluationAPIKey(context.Background(), ownedFD)
	require.NoError(t, err)
	require.Equal(t, "sk-fd-fixture", string(value))
	zeroBytes(value)
	require.Equal(t, make([]byte, len(value)), value)

	blockedReader, blockedWriter, err := os.Pipe()
	require.NoError(t, err)
	blockedFD, err := syscall.Dup(int(blockedReader.Fd()))
	require.NoError(t, err)
	require.NoError(t, blockedReader.Close())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = readEvaluationAPIKey(ctx, blockedFD)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, blockedWriter.Close())
}

func TestEvaluationErrorCodePrefersCancellationOverTransportLabel(t *testing.T) {
	t.Parallel()
	code := evaluationErrorCode(groupEvaluation{
		Error: context.Canceled,
		Result: service.ContentModerationDeepSeekEvaluationResult{
			Attempts: []service.ContentModerationReviewAttempt{{Outcome: "error", Error: "network"}},
		},
	})
	require.Equal(t, "canceled", code)
}

func TestReadableHistoryEvidencePrefersEveryWindowThenUsesLegacyRedactedExcerpt(t *testing.T) {
	t.Parallel()
	require.Equal(t, "first complete window\nsecond complete window", readableHistoryEvidence(historySourceRecord{
		InputExcerpt:    "must not override complete windows",
		EvidenceWindows: []historyEvidenceWindow{{Text: " first complete window "}, {Text: "second complete window"}},
	}))
	require.Equal(t, "legacy redacted excerpt", readableHistoryEvidence(historySourceRecord{
		InputExcerpt: " legacy redacted excerpt ",
	}))
	require.Empty(t, readableHistoryEvidence(historySourceRecord{}))
}

func TestPrepareHistoryHonorsCanceledContextBeforeReading(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))
	sourcePath := filepath.Join(directory, "source.ndjson")
	require.NoError(t, os.WriteFile(sourcePath, []byte("not read\n"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := prepareHistory(ctx, historyOptions{InputPath: sourcePath})
	require.ErrorIs(t, err, context.Canceled)
}

func TestAtomicWritersHonorCancellationAndNeverClobber(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o700))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledPath := filepath.Join(directory, "canceled.ndjson")
	err := writeNDJSONAtomic(canceled, canceledPath, []historyRecordResult{{ID: 1}})
	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, canceledPath)

	midWrite, cancelMidWrite := context.WithCancel(context.Background())
	midWritePath := filepath.Join(directory, "mid-write.json")
	err = writeAtomic(midWrite, midWritePath, func(writer io.Writer) error {
		if _, writeErr := io.WriteString(writer, "complete temporary payload"); writeErr != nil {
			return writeErr
		}
		cancelMidWrite()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.NoFileExists(t, midWritePath)

	const competitors = 16
	target := filepath.Join(directory, "winner.json")
	start := make(chan struct{})
	errorsByWriter := make(chan error, competitors)
	var wait sync.WaitGroup
	for index := 0; index < competitors; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errorsByWriter <- writeAtomic(context.Background(), target, func(writer io.Writer) error {
				_, err := io.WriteString(writer, string(rune('a'+index)))
				return err
			})
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByWriter)
	succeeded := 0
	for err := range errorsByWriter {
		if err == nil {
			succeeded++
			continue
		}
		require.ErrorContains(t, err, "refusing to overwrite")
	}
	require.Equal(t, 1, succeeded)
	require.Len(t, mustReadFile(t, target), 1)
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestPreflightRequiresPrivateOutputDirectory(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0o755))
	err := preflightHistoryOutputs(historyOptions{
		InputPath:   filepath.Join(directory, "source.ndjson"),
		ResultsPath: filepath.Join(directory, "results.ndjson"),
		SummaryPath: filepath.Join(directory, "summary.json"),
	})
	require.ErrorContains(t, err, "mode 0700")
}

func TestLatencySummaryUsesNearestRankPercentiles(t *testing.T) {
	t.Parallel()
	values := make([]int, 100)
	for index := range values {
		values[index] = index + 1
	}
	require.Equal(t, historyLatencySummary{P50: 50, P95: 95, P99: 99, Max: 100}, summarizeLatencies(values))
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	value, err := os.ReadFile(path)
	require.NoError(t, err)
	return value
}
