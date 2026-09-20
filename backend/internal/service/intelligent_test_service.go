package service

import (
	"context"
	"errors"
	"fmt"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var ErrIntelligentTestNotFound = infraerrors.NotFound("INTELLIGENT_TEST_NOT_FOUND", "test result not found")
var ErrIntelligentTestForbidden = infraerrors.Forbidden("INTELLIGENT_TEST_FORBIDDEN", "administrator access required")
var ErrIntelligentTestConflict = infraerrors.Conflict("INTELLIGENT_TEST_CONFLICT", "idempotency key was already used with different parameters")

const (
	pelicanScheduleInterval = 30 * time.Minute
	pelicanUserPageSize     = 32 // 30-minute runs in 08:00–24:00 Beijing, 24h window
)

func intelligentTestBad(message string) error {
	return infraerrors.BadRequest("INTELLIGENT_TEST_INVALID", message)
}

type IntelligentTestService struct {
	repo       IntelligentTestRepository
	runner     IntelligentTestRunner
	evaluators map[string]IntelligentTestEvaluator
	cancel     context.CancelFunc
	runCtx     context.Context
	lastAnimal string
	mu         sync.Mutex
	wg         sync.WaitGroup
}

func NewIntelligentTestService(repo IntelligentTestRepository, runner *AccountTestService) *IntelligentTestService {
	return &IntelligentTestService{repo: repo, runner: runner, evaluators: DefaultIntelligentTestEvaluators()}
}
func (s *IntelligentTestService) authorize(ctx context.Context, actor int64) error {
	ok, err := s.repo.IsAdmin(ctx, actor)
	if err != nil {
		return err
	}
	if !ok {
		return ErrIntelligentTestForbidden
	}
	return nil
}
func normalizeIntelligentFilter(f IntelligentTestFilter) IntelligentTestFilter {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 {
		f.PageSize = 24
	}
	if f.PageSize > 100 {
		f.PageSize = 100
	}
	return f
}
func (s *IntelligentTestService) Accounts(ctx context.Context, actor int64, f IntelligentTestFilter) (*IntelligentTestAccounts, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	return s.repo.Accounts(ctx, normalizeIntelligentFilter(f))
}
func (s *IntelligentTestService) Records(ctx context.Context, actor int64, f IntelligentTestFilter) (*IntelligentTestRecords, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	return s.repo.Records(ctx, normalizeIntelligentFilter(f))
}
func (s *IntelligentTestService) Get(ctx context.Context, actor, id int64) (*IntelligentTestRecord, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	record, err := s.repo.Get(ctx, id)
	if err == nil && record.ResultImage == "" && strings.Contains(record.Result, "<svg") {
		record.ResultImage, _, _ = PrepareIntelligentSVGPreview(record.Result)
	}
	return record, err
}
func (s *IntelligentTestService) Settings(ctx context.Context, actor int64) ([]IntelligentTestSetting, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	return s.repo.Settings(ctx)
}
func (s *IntelligentTestService) UpdateSetting(ctx context.Context, actor int64, setting *IntelligentTestSetting) error {
	if err := s.authorize(ctx, actor); err != nil {
		return err
	}
	if setting == nil || !regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`).MatchString(setting.TestType) {
		return intelligentTestBad("invalid test type")
	}
	cfg := &setting.Config
	cfg.Execution = nil // Only a runner may produce an execution snapshot.
	if err := validateIntelligentTestConfig(cfg, s.evaluators); err != nil {
		return err
	}
	return s.repo.UpdateSetting(ctx, actor, setting)
}

func validateIntelligentTestConfig(cfg *IntelligentTestConfig, evaluators map[string]IntelligentTestEvaluator) error {
	if err := validateIntelligentTextModel(cfg.Model); err != nil {
		return intelligentTestBad(err.Error())
	}
	cfg.Prompt = strings.TrimSpace(cfg.Prompt)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if len(cfg.Prompt) == 0 || utf8.RuneCountInString(cfg.Prompt) > 16000 || utf8.RuneCountInString(cfg.Model) > 200 || utf8.RuneCountInString(cfg.ExpectedAnswer) > 200 || utf8.RuneCountInString(cfg.AnswerUnit) > 32 {
		return intelligentTestBad("prompt/model/answer length is invalid")
	}
	if cfg.TimeoutSeconds < 30 || cfg.TimeoutSeconds > 600 {
		return intelligentTestBad("timeout_seconds must be 30–600")
	}
	if _, ok := evaluators[cfg.Evaluator]; !ok {
		return intelligentTestBad("unknown evaluator")
	}
	if cfg.Evaluator == "exact_answer" && strings.TrimSpace(cfg.ExpectedAnswer) == "" {
		return intelligentTestBad("expected_answer is required")
	}
	if cfg.AnswerType != "" && cfg.AnswerType != "auto" && cfg.AnswerType != "number" && cfg.AnswerType != "text" {
		return intelligentTestBad("invalid answer_type")
	}
	if cfg.AnswerFormat != "" && cfg.AnswerFormat != "answer_line" && cfg.AnswerFormat != "free_text" {
		return intelligentTestBad("invalid answer_format")
	}
	if cfg.AnswerUnitMode != "" && cfg.AnswerUnitMode != "none" && cfg.AnswerUnitMode != "configured" && cfg.AnswerUnitMode != "legacy" {
		return intelligentTestBad("invalid answer_unit_mode")
	}
	if cfg.Evaluator == "exact_answer" && cfg.AnswerUnitMode == "configured" && strings.TrimSpace(cfg.AnswerUnit) == "" {
		return intelligentTestBad("请选择不接受单位，或填写允许的单位")
	}
	if cfg.Evaluator == "exact_answer" && cfg.AnswerType == "number" {
		if _, ok := normalizeIntelligentNumber(cfg.ExpectedAnswer, intelligentAnswerUnit(*cfg)); !ok {
			return intelligentTestBad("数值题的标准答案不是有效数值")
		}
	}
	return nil
}
func (s *IntelligentTestService) Enqueue(ctx context.Context, actor int64, req IntelligentTestEnqueue) (*IntelligentTestEnqueued, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{16,100}$`).MatchString(req.IdempotencyKey) {
		return nil, intelligentTestBad("idempotency_key must contain 16–100 letters, digits, underscores or hyphens")
	}
	if len(req.AccountIDs) < 1 || len(req.AccountIDs) > 100 || len(req.TestTypes) < 1 || len(req.TestTypes) > 10 || len(req.AccountIDs)*len(req.TestTypes) > 200 {
		return nil, intelligentTestBad("select 1–100 accounts and at most 200 tests")
	}
	seen := map[int64]bool{}
	for _, id := range req.AccountIDs {
		if id < 1 || seen[id] {
			return nil, intelligentTestBad("account IDs must be positive and unique")
		}
		seen[id] = true
	}
	types := map[string]bool{}
	for _, kind := range req.TestTypes {
		if types[kind] {
			return nil, intelligentTestBad("test types must be unique")
		}
		types[kind] = true
	}
	for kind, model := range req.Models {
		if err := validateIntelligentTextModel(model); err != nil {
			return nil, intelligentTestBad(err.Error())
		}
		if !types[kind] || utf8.RuneCountInString(strings.TrimSpace(model)) > 200 {
			return nil, intelligentTestBad("model overrides must target selected test types and be at most 200 characters")
		}
	}
	out, err := s.repo.Enqueue(ctx, actor, req)
	if err != nil {
		return nil, err
	}
	s.startEnqueued(ctx, out)
	return out, nil
}

func (s *IntelligentTestService) startEnqueued(ctx context.Context, out *IntelligentTestEnqueued) {
	if out == nil {
		return
	}
	for i, rec := range out.Records {
		if rec == nil || rec.Status != "queued" {
			continue
		}
		started, err := s.repo.ClaimID(ctx, rec.ID)
		if err != nil {
			slog.Error("intelligent test start failed", "id", rec.ID, "error", err)
			continue
		}
		if started == nil {
			continue
		}
		s.spawn(started)
		compactIntelligentTestAPIRecord(started)
		out.Records[i] = started
	}
}

func compactIntelligentTestAPIRecord(r *IntelligentTestRecord) {
	if r == nil {
		return
	}
	r.Input = ""
	r.RawResponse = ""
	r.ConfigSnapshot = nil
	if len([]rune(r.Result)) > 600 {
		r.Result = string([]rune(r.Result)[:600]) + "…"
	}
}

func cloneIntelligentTestRecord(r *IntelligentTestRecord) *IntelligentTestRecord {
	if r == nil {
		return nil
	}
	out := *r
	if r.ConfigSnapshot != nil {
		cfg := *r.ConfigSnapshot
		out.ConfigSnapshot = &cfg
	}
	if r.Evaluation != nil {
		out.Evaluation = maps.Clone(r.Evaluation)
	}
	if r.AvailableAt != nil {
		t := *r.AvailableAt
		out.AvailableAt = &t
	}
	if r.StartedAt != nil {
		t := *r.StartedAt
		out.StartedAt = &t
	}
	if r.FinishedAt != nil {
		t := *r.FinishedAt
		out.FinishedAt = &t
	}
	if r.Score != nil {
		v := *r.Score
		out.Score = &v
	}
	return &out
}

func (s *IntelligentTestService) spawn(rec *IntelligentTestRecord) {
	clone := cloneIntelligentTestRecord(rec)
	s.mu.Lock()
	ctx := s.runCtx
	if ctx != nil && ctx.Err() == nil {
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.execute(ctx, clone)
		}()
		return
	}
	s.mu.Unlock()
	go s.execute(context.Background(), clone)
}
func (s *IntelligentTestService) UserPelicanTests(ctx context.Context, user int64, f IntelligentTestFilter) (*UserPelicanTests, error) {
	if user < 1 {
		return nil, ErrIntelligentTestForbidden
	}
	f = normalizeIntelligentFilter(f)
	f.Page = 1
	f.PageSize = pelicanUserPageSize
	return s.repo.UserPelicanTests(ctx, f)
}

func pelicanSlotKey(now time.Time) string {
	slot := now.UTC().Truncate(pelicanScheduleInterval)
	return "pelican-slot-" + slot.Format("200601021504")
}

func pelicanBeijingLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}

// pelicanInBeijingWindow is [08:00, 24:00) Asia/Shanghai (hour 8–23).
func pelicanInBeijingWindow(now time.Time) bool {
	return now.In(pelicanBeijingLocation()).Hour() >= 8
}

func pelicanNextBoundary(now time.Time) time.Time {
	local := now.In(pelicanBeijingLocation())
	return local.Truncate(pelicanScheduleInterval).Add(pelicanScheduleInterval)
}

func (s *IntelligentTestService) scheduledPelicanLoop(ctx context.Context) {
	defer s.wg.Done()
	s.purgeStalePelicanTests(ctx)
	s.runScheduledPelicanAt(ctx, time.Now())
	for {
		wait := time.Until(pelicanNextBoundary(time.Now()))
		if wait < time.Second {
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.purgeStalePelicanTests(ctx)
			s.runScheduledPelicanAt(ctx, time.Now())
		}
	}
}

func (s *IntelligentTestService) purgeStalePelicanTests(ctx context.Context) {
	if s == nil || s.repo == nil || ctx.Err() != nil {
		return
	}
	n, err := s.repo.DeleteStalePelicanTests(ctx)
	if err != nil {
		slog.Error("stale pelican cleanup failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("deleted pelican tests older than 24 hours", "count", n)
	}
}

func (s *IntelligentTestService) runScheduledPelican(ctx context.Context) {
	s.runScheduledPelicanAt(ctx, time.Now())
}

func (s *IntelligentTestService) runScheduledPelicanAt(ctx context.Context, now time.Time) {
	if s == nil || s.repo == nil || ctx.Err() != nil {
		return
	}
	if !pelicanInBeijingWindow(now) {
		return
	}
	settings, err := s.repo.Settings(ctx)
	if err != nil {
		slog.Error("scheduled pelican settings failed", "error", err)
		return
	}
	enabled := false
	for _, setting := range settings {
		if setting.TestType == "pelican" && setting.Enabled {
			enabled = true
			break
		}
	}
	if !enabled {
		return
	}
	actor, err := s.repo.FirstAdminUserID(ctx)
	if err != nil || actor < 1 {
		if err != nil {
			slog.Error("scheduled pelican admin lookup failed", "error", err)
		}
		return
	}
	ids, err := s.repo.ListGPTProOpenAIAccountIDs(ctx)
	if err != nil {
		slog.Error("scheduled pelican account list failed", "error", err)
		return
	}
	if len(ids) == 0 {
		slog.Info("scheduled pelican skipped: no GPT-PRO ChatGPT OAuth account with a live 292 gpt-6-astra ticket")
		return
	}
	s.mu.Lock()
	animal := pickIntelligentTestAnimal(s.lastAnimal)
	s.lastAnimal = animal
	s.mu.Unlock()
	picked := ids[randIntN(len(ids))]
	req := IntelligentTestEnqueue{
		AccountIDs:     []int64{picked},
		TestTypes:      []string{"pelican"},
		Models:         map[string]string{"pelican": intelligentTestDefaultCodexModel},
		Prompts:        map[string]string{"pelican": intelligentAnimalHTMLPrompt(animal)},
		IdempotencyKey: pelicanSlotKey(now),
	}
	out, err := s.repo.Enqueue(ctx, actor, req)
	if err != nil {
		slog.Error("scheduled pelican enqueue failed", "error", err, "account_id", picked, "animal", animal)
		return
	}
	s.startEnqueued(ctx, out)
}

func (s *IntelligentTestService) Capabilities(ctx context.Context, user int64, f IntelligentTestFilter) ([]AccountCapability, int64, error) {
	return s.repo.Capabilities(ctx, user, normalizeIntelligentFilter(f))
}
func (s *IntelligentTestService) PublicRecords(ctx context.Context, user int64, f IntelligentTestFilter) (*PublicAccountTests, error) {
	return s.repo.PublicRecords(ctx, user, normalizeIntelligentFilter(f))
}
func (s *IntelligentTestService) PublicGet(ctx context.Context, user, id int64) (*PublicAccountTest, error) {
	record, err := s.repo.PublicGet(ctx, user, id)
	if err == nil && record.ResultImage == "" && strings.Contains(record.Result, "<svg") {
		record.ResultImage, _, _ = PrepareIntelligentSVGPreview(record.Result)
	}
	return record, err
}
func (s *IntelligentTestService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.runCtx = ctx
	for i := 0; i < 2; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.work(ctx)
				}
			}
		}()
	}
	s.wg.Add(1)
	go s.scheduledPelicanLoop(ctx)
}
func (s *IntelligentTestService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}
func (s *IntelligentTestService) work(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		claimCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		record, err := s.repo.Claim(claimCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("intelligent test claim failed", "error", err)
			}
			return
		}
		if record == nil {
			return
		}
		s.spawn(record)
	}
}

func (s *IntelligentTestService) execute(ctx context.Context, record *IntelligentTestRecord) {
	s.run(ctx, record)
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer saveCancel()
	if record.Status == "queued" || record.Status == "running" || record.Status == "" {
		record.Status = "failed"
		if record.ErrorMessage == "" {
			record.ErrorMessage = "test did not reach a terminal status"
		}
	}
	if err := s.repo.Finish(saveCtx, record); err != nil {
		slog.Error("intelligent test result save failed; lease recovery will record interruption", "id", record.ID, "error", err)
	}
}

func (s *IntelligentTestService) run(ctx context.Context, r *IntelligentTestRecord) {
	start := time.Now()
	defer func() {
		r.DurationMS = time.Since(start).Milliseconds()
		if p := recover(); p != nil {
			r.Status = "failed"
			r.ErrorMessage = "test runner failed unexpectedly"
			slog.Error("intelligent test panic", "id", r.ID, "type", fmt.Sprintf("%T", p))
		}
		if r.Status != "completed" && r.Status != "queued" && r.Status != "running" {
			r.Evaluation = newIntelligentAssessment("")
			r.Evaluation["execution_status"], r.Evaluation["answer_verdict"], r.Evaluation["format_verdict"] = r.Status, "not_evaluated", "not_evaluated"
		}
	}()
	if r.ConfigSnapshot == nil {
		r.Status = "failed"
		r.ErrorMessage = "missing test configuration snapshot"
		return
	}
	timeout := time.Duration(r.ConfigSnapshot.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		err := s.runner.RunIntelligentTest(runCtx, r)
		if err == nil {
			evaluator, ok := s.evaluators[r.ConfigSnapshot.Evaluator]
			if !ok {
				r.Status = "failed"
				r.ErrorMessage = "evaluator unavailable"
				return
			}
			evaluated := evaluator.Evaluate(r.Result, *r.ConfigSnapshot)
			r.Status = evaluated.Status
			r.Score = evaluated.Score
			r.ResultImage = evaluated.Image
			r.Evaluation = evaluated.Detail
			return
		}
		if errors.Is(err, ErrIntelligentAccountBusy) {
			if runCtx.Err() != nil {
				r.Status = "rate_limited"
				r.ErrorMessage = "账号繁忙，测试超时未等到空闲"
				return
			}
			delay := 5 * time.Second
			var wait *TestAdmissionWaitError
			if errors.As(err, &wait) && wait != nil && wait.Until.After(time.Now()) {
				delay = time.Until(wait.Until)
			}
			timer := time.NewTimer(delay)
			select {
			case <-runCtx.Done():
				timer.Stop()
				r.Status = "rate_limited"
				r.ErrorMessage = "账号繁忙，测试超时未等到空闲"
				return
			case <-timer.C:
				continue
			}
		}
		if runCtx.Err() != nil {
			r.Status = "failed"
			r.ErrorMessage = "测试超时或服务中断；未自动重试以避免重复计费"
			return
		}
		if r.Status == "running" || r.Status == "" {
			r.Status = "failed"
		}
		if r.ErrorMessage == "" {
			r.ErrorMessage = err.Error()
		}
		r.Evaluation = newIntelligentAssessment(r.ConfigSnapshot.Evaluator)
		r.Evaluation["execution_status"], r.Evaluation["answer_verdict"], r.Evaluation["format_verdict"] = "failed", "not_evaluated", "not_evaluated"
		return
	}
}

func (s *IntelligentTestService) PreviewEvaluation(ctx context.Context, actor int64, output string, cfg IntelligentTestConfig) (*IntelligentTestRecord, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	if len(output) > 512<<10 {
		return nil, intelligentTestBad("试判输出不能超过 512 KiB")
	}
	if err := validateIntelligentTestConfig(&cfg, s.evaluators); err != nil {
		return nil, err
	}
	e := s.evaluators[cfg.Evaluator].Evaluate(output, cfg)
	return &IntelligentTestRecord{Status: e.Status, Score: e.Score, Result: output, ResultImage: e.Image, Evaluation: e.Detail}, nil
}

func (s *IntelligentTestService) Cancel(ctx context.Context, actor, id int64) (*IntelligentTestRecord, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	return s.repo.Cancel(ctx, actor, id)
}

func (s *IntelligentTestService) Reevaluate(ctx context.Context, actor, id int64) (*IntelligentTestRecord, error) {
	if err := s.authorize(ctx, actor); err != nil {
		return nil, err
	}
	return s.repo.Reevaluate(ctx, actor, id, func(r *IntelligentTestRecord) error {
		if r.ConfigSnapshot == nil || r.RawTruncated || strings.TrimSpace(r.Result) == "" {
			return intelligentTestBad("原始答复或配置不完整，无法自动重评")
		}
		if r.Status != "success" && r.Status != "completed" && r.Status != "suspected_degradation" && !(r.Status == "failed" && r.Evaluation["method"] != nil && r.ErrorMessage == "") {
			return intelligentTestBad("只能重评已经完成且具有完整答复的记录")
		}
		evaluator, ok := s.evaluators[r.ConfigSnapshot.Evaluator]
		if !ok {
			return intelligentTestBad("评估器不可用")
		}
		e := evaluator.Evaluate(r.Result, *r.ConfigSnapshot)
		r.Status, r.Score, r.ResultImage, r.Evaluation = e.Status, e.Score, e.Image, e.Detail
		return nil
	})
}
