//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type capturingBillingOutbox struct {
	commands []*BillingOutboxCommand
}

func (c *capturingBillingOutbox) Enqueue(_ context.Context, command *BillingOutboxCommand) (*BillingOutboxRecord, error) {
	copied := *command
	c.commands = append(c.commands, &copied)
	return &BillingOutboxRecord{ID: int64(len(c.commands)), Command: copied, Status: "pending"}, nil
}
func (c *capturingBillingOutbox) Claim(context.Context, string, int, time.Duration) ([]BillingOutboxRecord, error) {
	return nil, nil
}
func (c *capturingBillingOutbox) ClaimExpiredLeased(context.Context, string, int, time.Duration) ([]BillingOutboxRecord, error) {
	return nil, nil
}
func (c *capturingBillingOutbox) Retry(context.Context, int64, string, time.Time, string, bool) error {
	return nil
}
func (c *capturingBillingOutbox) Ack(context.Context, int64, string) error { return nil }
func (c *capturingBillingOutbox) Stats(context.Context) (BillingOutboxStats, error) {
	return BillingOutboxStats{}, nil
}
func (c *capturingBillingOutbox) CleanupTerminal(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type applyCountingBillingRepo struct {
	calls int
}

func (r *applyCountingBillingRepo) Apply(context.Context, *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.calls++
	return &UsageBillingApplyResult{Applied: true}, nil
}

func TestBillingOutboxDefaultPathEnqueuesInsteadOfApplying(t *testing.T) {
	outbox := &capturingBillingOutbox{}
	repo := &applyCountingBillingRepo{}
	user := &User{ID: 3, Email: "u@example.com"}
	apiKey := &APIKey{ID: 9, UserID: 3, Key: "sk-test"}
	account := &Account{ID: 2, Platform: PlatformOpenAI, Type: "oauth"}
	applied, err := applyUsageBilling(context.Background(), "req-1", &UsageLog{RequestID: "req-1", Model: "gpt-5.4"}, &postUsageBillingParams{
		Cost:     &CostBreakdown{TotalCost: 1.25, ActualCost: 1.25},
		User:     user,
		APIKey:   apiKey,
		Account:  account,
		Platform: PlatformOpenAI,
	}, &billingDeps{}, repo, outbox)
	require.NoError(t, err)
	require.False(t, applied)
	require.Zero(t, repo.calls)
	require.Len(t, outbox.commands, 1)
	require.Equal(t, "req-1", outbox.commands[0].AttemptID)
	require.Equal(t, int64(9), outbox.commands[0].APIKeyID)
	require.Equal(t, 1.25, outbox.commands[0].Billing.BalanceCost)
}

func TestBillingOutboxAbsentFallsBackToSyncApply(t *testing.T) {
	repo := &applyCountingBillingRepo{}
	user := &User{ID: 3}
	apiKey := &APIKey{ID: 9, UserID: 3}
	account := &Account{ID: 2}
	applied, err := applyUsageBilling(context.Background(), "req-2", &UsageLog{RequestID: "req-2"}, &postUsageBillingParams{
		Cost:    &CostBreakdown{TotalCost: 0, ActualCost: 0},
		User:    user,
		APIKey:  apiKey,
		Account: account,
	}, &billingDeps{deferredService: &DeferredService{}}, repo, nil)
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, 1, repo.calls)
}
