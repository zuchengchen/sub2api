//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// batchBillingFixture 是一个用户及其计费相关的行：两个 Key（A 有额度）、两个支持额度的账号
// （P 设总额度与日额度，Q 只设周额度）和一个订阅。
type batchBillingFixture struct {
	userID         int64
	keyA, keyB     int64
	accountP       int64
	accountQ       int64
	subscriptionID int64
}

func newBatchBillingFixture(t *testing.T, client *dbent.Client, balance float64) batchBillingFixture {
	t.Helper()
	suffix := uuid.NewString()
	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("usage-billing-batch-%s@example.com", suffix),
		PasswordHash: "hash",
		Balance:      balance,
	})
	group := mustCreateGroup(t, client, &service.Group{
		Name:             "usage-billing-batch-group-" + suffix,
		Platform:         service.PlatformAnthropic,
		SubscriptionType: service.SubscriptionTypeSubscription,
	})
	keyA := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID, Key: "sk-usage-billing-batch-a-" + suffix, Name: "batch-a", Quota: 5,
	})
	keyB := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID, Key: "sk-usage-billing-batch-b-" + suffix, Name: "batch-b",
	})
	accountP := mustCreateAccount(t, client, &service.Account{
		Name:  "usage-billing-batch-p-" + suffix,
		Type:  service.AccountTypeAPIKey,
		Extra: map[string]any{"quota_limit": 6.0, "quota_daily_limit": 4.0},
	})
	accountQ := mustCreateAccount(t, client, &service.Account{
		Name:  "usage-billing-batch-q-" + suffix,
		Type:  service.AccountTypeAPIKey,
		Extra: map[string]any{"quota_weekly_limit": 3.0},
	})
	subscription := mustCreateSubscription(t, client, &service.UserSubscription{UserID: user.ID, GroupID: group.ID})
	return batchBillingFixture{
		userID: user.ID, keyA: keyA.ID, keyB: keyB.ID,
		accountP: accountP.ID, accountQ: accountQ.ID, subscriptionID: subscription.ID,
	}
}

// batchBillingScript 描述一组命令；key/account 用 'A'/'B'、'P'/'Q' 指代夹具中的行。
type batchBillingStep struct {
	key          byte
	account      byte
	balance      float64
	subscription float64
	keyQuota     float64
	keyRate      float64
	accountQuota float64
}

func (f batchBillingFixture) commands(prefix string, steps []batchBillingStep) []*service.UsageBillingCommand {
	cmds := make([]*service.UsageBillingCommand, len(steps))
	for i, s := range steps {
		cmd := &service.UsageBillingCommand{
			RequestID:           fmt.Sprintf("%s-%d", prefix, i),
			UserID:              f.userID,
			AccountType:         service.AccountTypeAPIKey,
			BalanceCost:         s.balance,
			APIKeyQuotaCost:     s.keyQuota,
			APIKeyRateLimitCost: s.keyRate,
			AccountQuotaCost:    s.accountQuota,
		}
		cmd.APIKeyID = f.keyA
		if s.key == 'B' {
			cmd.APIKeyID = f.keyB
		}
		cmd.AccountID = f.accountP
		if s.account == 'Q' {
			cmd.AccountID = f.accountQ
		}
		if s.subscription > 0 {
			subscriptionID := f.subscriptionID
			cmd.SubscriptionID = &subscriptionID
			cmd.SubscriptionCost = s.subscription
		}
		cmds[i] = cmd
	}
	return cmds
}

type batchBillingState struct {
	balance                        string
	keyAUsed, keyAStatus           string
	keyAUsage5h, keyAUsage1d       string
	keyBUsage5h, keyBUsage7d       string
	pUsed, pDailyUsed, pWeeklyUsed string
	qUsed, qDailyUsed, qWeeklyUsed string
	subDaily, subWeekly, subMonth  string
	dedupRows                      int
	pOutbox, qOutbox               int
}

func readBatchBillingState(t *testing.T, ctx context.Context, f batchBillingFixture) batchBillingState {
	t.Helper()
	var s batchBillingState
	q := func(dst any, query string, args ...any) {
		require.NoError(t, integrationDB.QueryRowContext(ctx, query, args...).Scan(dst))
	}
	q(&s.balance, "SELECT balance::text FROM users WHERE id = $1", f.userID)
	q(&s.keyAUsed, "SELECT quota_used::text FROM api_keys WHERE id = $1", f.keyA)
	q(&s.keyAStatus, "SELECT status FROM api_keys WHERE id = $1", f.keyA)
	q(&s.keyAUsage5h, "SELECT usage_5h::text FROM api_keys WHERE id = $1", f.keyA)
	q(&s.keyAUsage1d, "SELECT usage_1d::text FROM api_keys WHERE id = $1", f.keyA)
	q(&s.keyBUsage5h, "SELECT usage_5h::text FROM api_keys WHERE id = $1", f.keyB)
	q(&s.keyBUsage7d, "SELECT usage_7d::text FROM api_keys WHERE id = $1", f.keyB)
	accountUsed := func(id int64, field string) string {
		var v string
		q(&v, "SELECT COALESCE((extra->>$2)::numeric, 0)::text FROM accounts WHERE id = $1", id, field)
		return v
	}
	s.pUsed, s.pDailyUsed, s.pWeeklyUsed = accountUsed(f.accountP, "quota_used"), accountUsed(f.accountP, "quota_daily_used"), accountUsed(f.accountP, "quota_weekly_used")
	s.qUsed, s.qDailyUsed, s.qWeeklyUsed = accountUsed(f.accountQ, "quota_used"), accountUsed(f.accountQ, "quota_daily_used"), accountUsed(f.accountQ, "quota_weekly_used")
	q(&s.subDaily, "SELECT daily_usage_usd::text FROM user_subscriptions WHERE id = $1", f.subscriptionID)
	q(&s.subWeekly, "SELECT weekly_usage_usd::text FROM user_subscriptions WHERE id = $1", f.subscriptionID)
	q(&s.subMonth, "SELECT monthly_usage_usd::text FROM user_subscriptions WHERE id = $1", f.subscriptionID)
	q(&s.dedupRows, "SELECT COUNT(*) FROM usage_billing_dedup WHERE api_key_id IN ($1, $2)", f.keyA, f.keyB)
	q(&s.pOutbox, "SELECT COUNT(*) FROM scheduler_outbox WHERE event_type = $1 AND account_id = $2", service.SchedulerOutboxEventAccountChanged, f.accountP)
	q(&s.qOutbox, "SELECT COUNT(*) FROM scheduler_outbox WHERE event_type = $1 AND account_id = $2", service.SchedulerOutboxEventAccountChanged, f.accountQ)
	return s
}

func TestUsageBillingRepositoryApplyBatch_MatchesSequentialApply(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := &usageBillingRepository{db: integrationDB}

	// 余额 10：第 6 条起透支；Key A 额度 5 在第 5 条耗尽；账号 P 日额度 4 在第 5 条、总额度 6 在第 6 条越线；
	// 账号 Q 周额度 3 在第 3 条恰好达到；第 4 条走订阅。
	steps := []batchBillingStep{
		{key: 'A', account: 'P', balance: 1.5, keyQuota: 1.5, keyRate: 1.5, accountQuota: 1.5},
		{key: 'A', account: 'P', balance: 2.25, keyQuota: 2.25, keyRate: 2.25, accountQuota: 2.25},
		{key: 'B', account: 'Q', balance: 3, keyRate: 3, accountQuota: 3},
		{key: 'A', account: 'P', subscription: 1, keyQuota: 1},
		{key: 'A', account: 'P', balance: 4, keyQuota: 0.5, accountQuota: 0.5},
		{key: 'B', account: 'P', balance: 0.75, keyRate: 0.12345678, accountQuota: 2},
		{key: 'A', account: 'Q', balance: 0.1, keyQuota: 0.3, accountQuota: 0.00000001},
		{key: 'B', account: 'Q', balance: 0.00000003, keyRate: 0.1},
	}

	sequential := newBatchBillingFixture(t, client, 10)
	batched := newBatchBillingFixture(t, client, 10)

	seqCmds := sequential.commands("seq-"+uuid.NewString(), steps)
	seqResults := make([]*service.UsageBillingApplyResult, len(seqCmds))
	for i, cmd := range seqCmds {
		res, err := repo.Apply(ctx, cmd)
		require.NoError(t, err, "sequential step %d", i)
		seqResults[i] = res
	}

	queue := &billingOutboxRepository{db: integrationDB}
	items := enqueueBillingBatch(t, queue, "billing-worker-parity", batched.commands("batch-"+uuid.NewString(), steps))
	outcomes, err := repo.ApplyBatchAndStageOutboxFinalizations(ctx, items)
	require.NoError(t, err)
	require.Len(t, outcomes, len(steps))

	for i := range steps {
		require.NoError(t, outcomes[i].Err, "step %d", i)
		require.Equal(t, seqResults[i], outcomes[i].Result, "step %d", i)
	}

	seqState := readBatchBillingState(t, ctx, sequential)
	batchState := readBatchBillingState(t, ctx, batched)
	// 逐条结算时每次越线各写一条 outbox，合并结算每个账号至多一条；两边都必须至少写一条。
	require.Positive(t, seqState.pOutbox)
	require.Positive(t, batchState.pOutbox)
	require.Positive(t, seqState.qOutbox)
	require.Positive(t, batchState.qOutbox)
	require.Equal(t, 1, batchState.pOutbox)
	require.Equal(t, 1, batchState.qOutbox)
	seqState.pOutbox, seqState.qOutbox, batchState.pOutbox, batchState.qOutbox = 0, 0, 0, 0
	require.Equal(t, seqState, batchState)
	require.Equal(t, len(steps), batchState.dedupRows)
	require.Equal(t, service.StatusAPIKeyQuotaExhausted, batchState.keyAStatus)
}

func TestUsageBillingRepositoryApplyBatch_DuplicateAndConflictingRequests(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := &usageBillingRepository{db: integrationDB}
	f := newBatchBillingFixture(t, client, 10)

	prefix := "dup-" + uuid.NewString()
	settled := f.commands(prefix+"-settled", []batchBillingStep{{key: 'B', balance: 1}})[0]
	conflicted := f.commands(prefix+"-conflicted", []batchBillingStep{{key: 'B', balance: 2}})[0]
	for _, cmd := range []*service.UsageBillingCommand{settled, conflicted} {
		res, err := repo.Apply(ctx, cmd)
		require.NoError(t, err)
		require.True(t, res.Applied)
	}

	retry := f.commands(prefix+"-settled", []batchBillingStep{{key: 'B', balance: 1}})[0]
	changed := f.commands(prefix+"-conflicted", []batchBillingStep{{key: 'B', balance: 2.5}})[0]
	fresh := f.commands(prefix+"-fresh", []batchBillingStep{{key: 'B', balance: 0.5}})[0]

	outcomes, err := repo.ApplyBatch(ctx, []*service.UsageBillingCommand{retry, changed, fresh})
	require.NoError(t, err)
	require.NoError(t, outcomes[0].Err)
	require.False(t, outcomes[0].Result.Applied)
	require.ErrorIs(t, outcomes[1].Err, service.ErrUsageBillingRequestConflict)
	require.Nil(t, outcomes[1].Result)
	require.NoError(t, outcomes[2].Err)
	require.True(t, outcomes[2].Result.Applied)
	require.InDelta(t, 6.5, *outcomes[2].Result.NewBalance, 1e-9)

	state := readBatchBillingState(t, ctx, f)
	require.Equal(t, "6.50000000", state.balance)
	require.Equal(t, 3, state.dedupRows)
}

func TestUsageBillingRepositoryApplyBatch_ArchivedRequestsAreNotReapplied(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := &usageBillingRepository{db: integrationDB}
	f := newBatchBillingFixture(t, client, 10)

	prefix := "archived-" + uuid.NewString()
	archived := f.commands(prefix+"-same", []batchBillingStep{{key: 'B', balance: 1}})[0]
	archivedChanged := f.commands(prefix+"-changed", []batchBillingStep{{key: 'B', balance: 2}})[0]
	fresh := f.commands(prefix+"-fresh", []batchBillingStep{{key: 'B', balance: 3}})[0]
	for _, cmd := range []*service.UsageBillingCommand{archived, archivedChanged, fresh} {
		cmd.Normalize()
	}
	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO usage_billing_dedup_archive (request_id, api_key_id, request_fingerprint, created_at)
		VALUES ($1, $2, $3, NOW()), ($4, $5, $6, NOW())
	`, archived.RequestID, archived.APIKeyID, archived.RequestFingerprint,
		archivedChanged.RequestID, archivedChanged.APIKeyID, "different-fingerprint")
	require.NoError(t, err)

	outcomes, err := repo.ApplyBatch(ctx, []*service.UsageBillingCommand{archived, archivedChanged, fresh})
	require.NoError(t, err)
	require.NoError(t, outcomes[0].Err)
	require.False(t, outcomes[0].Result.Applied)
	require.ErrorIs(t, outcomes[1].Err, service.ErrUsageBillingRequestConflict)
	require.True(t, outcomes[2].Result.Applied)

	state := readBatchBillingState(t, ctx, f)
	require.Equal(t, "7.00000000", state.balance)
	require.Equal(t, 1, state.dedupRows, "claims of archived requests must not remain")
}

func TestUsageBillingRepositoryApplyBatch_RollsBackWholeBatchOnError(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := &usageBillingRepository{db: integrationDB}

	t.Run("deleted user", func(t *testing.T) {
		f := newBatchBillingFixture(t, client, 10)
		_, err := integrationDB.ExecContext(ctx, "UPDATE users SET deleted_at = NOW() WHERE id = $1", f.userID)
		require.NoError(t, err)

		_, err = repo.ApplyBatch(ctx, f.commands("deleted-user-"+uuid.NewString(), []batchBillingStep{
			{key: 'A', account: 'P', balance: 1, keyQuota: 1, accountQuota: 1},
			{key: 'B', balance: 1},
		}))
		require.ErrorIs(t, err, service.ErrUserNotFound)
		state := readBatchBillingState(t, ctx, f)
		require.Zero(t, state.dedupRows)
		require.Equal(t, "0", state.pUsed)
	})

	t.Run("missing subscription", func(t *testing.T) {
		f := newBatchBillingFixture(t, client, 10)
		_, err := integrationDB.ExecContext(ctx, "UPDATE user_subscriptions SET deleted_at = NOW() WHERE id = $1", f.subscriptionID)
		require.NoError(t, err)

		_, err = repo.ApplyBatch(ctx, f.commands("missing-sub-"+uuid.NewString(), []batchBillingStep{
			{key: 'B', balance: 1},
			{key: 'A', subscription: 1},
		}))
		require.ErrorIs(t, err, service.ErrSubscriptionNotFound)
		state := readBatchBillingState(t, ctx, f)
		require.Zero(t, state.dedupRows)
		require.Equal(t, "10.00000000", state.balance)
	})

	t.Run("invalid batches", func(t *testing.T) {
		f := newBatchBillingFixture(t, client, 10)
		other := newBatchBillingFixture(t, client, 10)
		prefix := "invalid-" + uuid.NewString()

		mixed := append(f.commands(prefix+"-f", []batchBillingStep{{key: 'B', balance: 1}}),
			other.commands(prefix+"-o", []batchBillingStep{{key: 'B', balance: 1}})...)
		_, err := repo.ApplyBatch(ctx, mixed)
		require.ErrorIs(t, err, errUsageBillingBatchMixedUsers)

		dup := f.commands(prefix+"-dup", []batchBillingStep{{key: 'B', balance: 1}})
		dup = append(dup, f.commands(prefix+"-dup", []batchBillingStep{{key: 'B', balance: 1}})...)
		_, err = repo.ApplyBatch(ctx, dup)
		require.ErrorIs(t, err, errUsageBillingBatchDuplicateRequest)

		require.Zero(t, readBatchBillingState(t, ctx, f).dedupRows)
		require.Zero(t, readBatchBillingState(t, ctx, other).dedupRows)
	})
}

func enqueueBillingBatch(t *testing.T, queue service.BillingOutboxRepository, workerID string, cmds []*service.UsageBillingCommand) []service.UsageBillingBatchItem {
	t.Helper()
	items := make([]service.UsageBillingBatchItem, len(cmds))
	for i, cmd := range cmds {
		record, err := queue.Enqueue(t.Context(), &service.BillingOutboxCommand{AttemptID: cmd.RequestID, Billing: *cmd})
		require.NoError(t, err)
		items[i] = service.UsageBillingBatchItem{
			Command: record.Command.Billing,
			Binding: service.UsageBillingOutboxBinding{OutboxID: record.ID, WorkerID: workerID},
		}
		t.Cleanup(func() {
			_, err := integrationDB.ExecContext(context.Background(), "DELETE FROM billing_attempt_outbox WHERE id = $1", record.ID)
			require.NoError(t, err)
		})
	}
	records, err := queue.Claim(t.Context(), workerID, 1000, time.Minute)
	require.NoError(t, err)
	claimed := make(map[int64]bool, len(records))
	for _, record := range records {
		claimed[record.ID] = true
	}
	for _, item := range items {
		require.True(t, claimed[item.Binding.OutboxID])
	}
	return items
}

func TestUsageBillingOutboxBatch_ConcurrentSameUserReplay(t *testing.T) {
	ctx := t.Context()
	client := testEntClient(t)
	repo := &usageBillingRepository{db: integrationDB}
	queue := &billingOutboxRepository{db: integrationDB}

	const n = 60
	steps := make([]batchBillingStep, n)
	for i := range steps {
		steps[i] = batchBillingStep{key: 'B', account: 'P', balance: float64(i%7+1) * 0.01, keyRate: 0.01, accountQuota: 0.001}
	}
	f := newBatchBillingFixture(t, client, 1000)
	cmds := f.commands("outbox-batch-"+uuid.NewString(), steps)
	first := enqueueBillingBatch(t, queue, "billing-worker-first", cmds[:n/2])
	second := enqueueBillingBatch(t, queue, "billing-worker-second", cmds[n/2:])

	results := make([][]service.UsageBillingBatchOutcome, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, batch := range [][]service.UsageBillingBatchItem{first, second} {
		wg.Go(func() {
			<-start
			results[i], errs[i] = repo.ApplyBatchAndStageOutboxFinalizations(ctx, batch)
		})
	}
	close(start)
	wg.Wait()

	var total float64
	seen := make(map[float64]bool, n)
	for i, batch := range results {
		require.NoError(t, errs[i])
		require.Len(t, batch, n/2)
		for _, outcome := range batch {
			require.NoError(t, outcome.Err)
			require.True(t, outcome.Result.Applied)
			require.False(t, outcome.Result.BalanceOverdrafted)
			balance := *outcome.Result.NewBalance
			require.False(t, seen[balance])
			seen[balance] = true
		}
	}
	for _, cmd := range cmds {
		total += cmd.BalanceCost
	}
	state := readBatchBillingState(t, ctx, f)
	require.Equal(t, fmt.Sprintf("%.8f", 1000-total), state.balance)
	require.Equal(t, n, state.dedupRows)
	require.Equal(t, "0.06", state.pUsed)

	// A replacement worker finalizes the committed outcomes after a restart.
	// The prior apply worker cannot replay the debit or claim finalization.
	restarted := &billingOutboxRepository{db: integrationDB}
	staged, err := restarted.ClaimFinalization(ctx, "billing-worker-restarted", 1000, time.Minute)
	require.NoError(t, err)
	stagedByID := make(map[int64]service.BillingOutboxRecord, len(staged))
	for _, record := range staged {
		stagedByID[record.ID] = record
	}
	for i, batch := range [][]service.UsageBillingBatchItem{first, second} {
		for j, item := range batch {
			_, err := repo.ApplyAndStageOutboxFinalization(ctx, &item.Command, item.Binding)
			require.ErrorIs(t, err, service.ErrBillingOutboxClaimLost)
			record, found := stagedByID[item.Binding.OutboxID]
			require.True(t, found)
			require.Equal(t, results[i][j].Result, record.ApplyResult)
			require.NoError(t, restarted.AckFinalization(ctx, record.ID, "billing-worker-restarted"))
		}
	}
	require.Equal(t, state, readBatchBillingState(t, ctx, f))
}

func TestUsageBillingOutboxBatch_RetriesAfterCancellationAndIsolatesInvalidEntry(t *testing.T) {
	ctx := t.Context()
	client := testEntClient(t)
	repo := &usageBillingRepository{db: integrationDB}
	queue := &billingOutboxRepository{db: integrationDB}
	f := newBatchBillingFixture(t, client, 10)
	steps := []batchBillingStep{
		{key: 'A', account: 'P', balance: 1.000000005, keyQuota: 1.000000005},
		{key: 'B', subscription: 1},
		{key: 'B', balance: 0.2},
	}
	cmds := f.commands("outbox-retry-"+uuid.NewString(), steps)
	items := enqueueBillingBatch(t, queue, "billing-worker-prior", cmds)
	// Interrupt an in-flight apply while a transaction holds the target row.
	blocker, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, "SELECT 1 FROM users WHERE id = $1 FOR UPDATE", f.userID)
	require.NoError(t, err)
	applyCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	outcomes, err := repo.ApplyBatchAndStageOutboxFinalizations(applyCtx, items)
	require.NoError(t, err)
	for _, outcome := range outcomes {
		require.Error(t, outcome.Err)
	}
	require.NoError(t, blocker.Rollback())
	require.Equal(t, "10.00000000", readBatchBillingState(t, ctx, f).balance)
	_, err = integrationDB.ExecContext(ctx, "UPDATE user_subscriptions SET deleted_at = NOW() WHERE id = $1", f.subscriptionID)
	require.NoError(t, err)

	// Expired queue leases let a replacement worker resume the batch.
	for _, item := range items {
		_, err := integrationDB.ExecContext(ctx, "UPDATE billing_attempt_outbox SET lease_until = NOW() - INTERVAL '1 second' WHERE id = $1", item.Binding.OutboxID)
		require.NoError(t, err)
	}
	_, err = queue.ClaimExpiredLeased(ctx, "billing-worker-resumed", 1000, time.Minute)
	require.NoError(t, err)
	for i := range items {
		items[i].Binding.WorkerID = "billing-worker-resumed"
	}
	outcomes, err = repo.ApplyBatchAndStageOutboxFinalizations(ctx, items)
	require.NoError(t, err)
	require.NoError(t, outcomes[0].Err)
	require.True(t, outcomes[0].Result.Applied)
	require.ErrorIs(t, outcomes[1].Err, service.ErrSubscriptionNotFound)
	require.NoError(t, outcomes[2].Err)
	require.True(t, outcomes[2].Result.Applied)
	state := readBatchBillingState(t, ctx, f)
	require.Equal(t, "8.79999999", state.balance)
	require.Equal(t, "1.00000001", state.keyAUsed)
	require.Equal(t, 2, state.dedupRows)
}

func TestUsageBillingRepositoryApplyBatch_RejectsNonFiniteAmounts(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := &usageBillingRepository{db: integrationDB}
	f := newBatchBillingFixture(t, client, 10)

	cmds := f.commands("nonfinite-"+uuid.NewString(), []batchBillingStep{
		{key: 'B', balance: 1},
		{key: 'B', account: 'P', balance: 1, accountQuota: 1},
	})
	cmds[1].AccountQuotaCost = math.NaN()

	require.NotPanics(t, func() {
		_, err := repo.ApplyBatch(ctx, cmds)
		require.ErrorIs(t, err, errUsageBillingBatchNonFiniteAmount)
	})
	require.Zero(t, readBatchBillingState(t, ctx, f).dedupRows)
}

// 计费合并事务按 id 升序持有多个账号行锁；BatchUpdateLastUsed 也按 id 升序加锁，两者交错时不形成死锁。
func TestAccountRepositoryBatchUpdateLastUsed_LocksRowsInIDOrder(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	accounts := newAccountRepositoryWithSQL(client, integrationDB, nil)

	low := mustCreateAccount(t, client, &service.Account{Name: "last-used-order-low-" + uuid.NewString(), Type: service.AccountTypeAPIKey})
	high := mustCreateAccount(t, client, &service.Account{Name: "last-used-order-high-" + uuid.NewString(), Type: service.AccountTypeAPIKey})
	require.Less(t, low.ID, high.ID)
	_, err := integrationDB.ExecContext(ctx, "UPDATE accounts SET temp_unschedulable_until = NOW() + INTERVAL '1 hour' WHERE id = $1", high.ID)
	require.NoError(t, err)

	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET updated_at = NOW() WHERE id = $1", low.ID)
	require.NoError(t, err)

	now := time.Now()
	flushed := make(chan error, 1)
	go func() {
		flushed <- accounts.BatchUpdateLastUsed(ctx, map[int64]time.Time{low.ID: now, high.ID: now})
	}()
	time.Sleep(300 * time.Millisecond)

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = tx.ExecContext(lockCtx, "UPDATE accounts SET updated_at = NOW() WHERE id = $1", high.ID)
	require.NoError(t, err, "the flush must not hold the higher-id row while waiting for the lower-id row")
	require.NoError(t, tx.Commit())

	select {
	case err := <-flushed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("BatchUpdateLastUsed did not finish")
	}
	var lastUsed sql.NullTime
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT last_used_at FROM accounts WHERE id = $1", high.ID).Scan(&lastUsed))
	require.True(t, lastUsed.Valid)
}

func TestAccountRepositoryBatchUpdateLastUsed_DoesNotBlockForeignKeyShareLocks(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	accounts := newAccountRepositoryWithSQL(client, integrationDB, nil)

	acc := mustCreateAccount(t, client, &service.Account{Name: "last-used-fk-" + uuid.NewString(), Type: service.AccountTypeAPIKey})

	// 外键检查对被引用行加 FOR KEY SHARE。
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "SELECT id FROM accounts WHERE id = $1 FOR KEY SHARE", acc.ID)
	require.NoError(t, err)

	flushCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	require.NoError(t, accounts.BatchUpdateLastUsed(flushCtx, map[int64]time.Time{acc.ID: time.Now()}))
	require.NoError(t, tx.Commit())

	var lastUsed sql.NullTime
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT last_used_at FROM accounts WHERE id = $1", acc.ID).Scan(&lastUsed))
	require.True(t, lastUsed.Valid)
}
