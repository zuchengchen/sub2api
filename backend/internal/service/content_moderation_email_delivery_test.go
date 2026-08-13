package service

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type contentModerationEmailClaimTestRepo struct {
	contentModerationTestRepo
	mu                  sync.Mutex
	exists              bool
	claimed             bool
	status              string
	failNextCompletion  bool
	completionCallCount int
}

func (r *contentModerationEmailClaimTestRepo) ClaimLogEmailDelivery(context.Context, int64) (ContentModerationEmailDeliveryClaim, error) {
	return r.claim()
}

func (r *contentModerationEmailClaimTestRepo) ClaimLogEmailDeliveryByArchiveID(context.Context, string) (ContentModerationEmailDeliveryClaim, error) {
	return r.claim()
}

func (r *contentModerationEmailClaimTestRepo) claim() (ContentModerationEmailDeliveryClaim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.exists {
		return ContentModerationEmailDeliveryClaim{}, nil
	}
	if r.claimed {
		return ContentModerationEmailDeliveryClaim{Exists: true, Status: r.status}, nil
	}
	r.claimed = true
	r.status = "claimed"
	return ContentModerationEmailDeliveryClaim{Exists: true, Claimed: true, Status: r.status}, nil
}

func (r *contentModerationEmailClaimTestRepo) CompleteLogEmailDelivery(_ context.Context, _ int64, sent bool) error {
	return r.complete(sent)
}

func (r *contentModerationEmailClaimTestRepo) CompleteLogEmailDeliveryByArchiveID(_ context.Context, _ string, sent bool) error {
	return r.complete(sent)
}

func (r *contentModerationEmailClaimTestRepo) complete(sent bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completionCallCount++
	if r.failNextCompletion {
		r.failNextCompletion = false
		return errors.New("temporary completion failure")
	}
	if sent {
		r.status = "sent"
	} else {
		r.status = "failed"
	}
	return nil
}

func (r *contentModerationEmailClaimTestRepo) snapshot() (string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.completionCallCount
}

func TestContentModerationEmailDeliveryConcurrentClaimSendsOnce(t *testing.T) {
	repo := &contentModerationEmailClaimTestRepo{exists: true}
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	var sends atomic.Int64
	var stateErrors atomic.Int64

	const workers = 32
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			outcome := svc.deliverClaimedContentModerationEmail(context.Background(), &ContentModerationLog{ID: 17}, func() error {
				sends.Add(1)
				return nil
			})
			if outcome.StateErr != nil {
				stateErrors.Add(1)
			}
		}()
	}
	wg.Wait()

	require.Zero(t, stateErrors.Load())
	require.Equal(t, int64(1), sends.Load())
	status, completions := repo.snapshot()
	require.Equal(t, "sent", status)
	require.Equal(t, 1, completions)
}

func TestContentModerationEmailDeliveryCompletionFailureNeverResends(t *testing.T) {
	repo := &contentModerationEmailClaimTestRepo{exists: true, failNextCompletion: true}
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	log := &ContentModerationLog{ID: 23}
	var sends atomic.Int64
	send := func() error {
		sends.Add(1)
		return nil
	}

	first := svc.deliverClaimedContentModerationEmail(context.Background(), log, send)
	require.Error(t, first.StateErr)
	require.True(t, first.CompletionRequired)
	require.True(t, first.Sent)
	require.False(t, first.SendRequired)

	second := svc.deliverClaimedContentModerationEmail(context.Background(), log, send)
	require.NoError(t, second.StateErr)
	require.False(t, second.SendRequired)
	require.Equal(t, int64(1), sends.Load(), "a claimed delivery must never be sent again")

	require.NoError(t, repo.CompleteLogEmailDelivery(context.Background(), log.ID, first.Sent))
	status, completions := repo.snapshot()
	require.Equal(t, "sent", status)
	require.Equal(t, 2, completions)
}

func TestContentModerationEmailDeliveryFailureIsTerminalAndNotRetried(t *testing.T) {
	repo := &contentModerationEmailClaimTestRepo{exists: true}
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	log := &ContentModerationLog{ID: 29}
	var sends atomic.Int64
	send := func() error {
		sends.Add(1)
		return errors.New("SMTP result unknown")
	}

	first := svc.deliverClaimedContentModerationEmail(context.Background(), log, send)
	require.Error(t, first.DeliveryErr)
	require.NoError(t, first.StateErr)
	require.False(t, first.SendRequired)
	require.False(t, first.CompletionRequired)

	second := svc.deliverClaimedContentModerationEmail(context.Background(), log, send)
	require.NoError(t, second.StateErr)
	require.False(t, second.SendRequired)
	require.Equal(t, int64(1), sends.Load())
	status, _ := repo.snapshot()
	require.Equal(t, "failed", status)
}

func TestContentModerationEmailDeliveryWaitsForPersistedLog(t *testing.T) {
	repo := &contentModerationEmailClaimTestRepo{}
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	var sends atomic.Int64

	outcome := svc.deliverClaimedContentModerationEmail(context.Background(), &ContentModerationLog{ArchiveID: "b411db6f-39c3-4ff7-acfb-ecb860d6a68b"}, func() error {
		sends.Add(1)
		return nil
	})

	require.ErrorIs(t, outcome.StateErr, sql.ErrNoRows)
	require.True(t, outcome.SendRequired)
	require.Zero(t, sends.Load())
}
