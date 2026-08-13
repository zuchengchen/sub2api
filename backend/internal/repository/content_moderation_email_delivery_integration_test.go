//go:build integration

package repository

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestContentModerationEmailDeliveryClaimIsAtomicAcrossConnections(t *testing.T) {
	ctx := context.Background()
	repo := NewContentModerationRepository(integrationDB)
	log := &service.ContentModerationLog{
		RequestID: uuid.NewString(), Action: service.ContentModerationActionCyberPolicy,
		Flagged: true, DispositionStatus: "disabled", DispositionTarget: "user",
		DispositionTransitioned: true,
	}
	require.NoError(t, repo.CreateLog(ctx, log))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DELETE FROM content_moderation_logs WHERE id = $1`, log.ID)
	})

	deliveryRepo := repo.(service.ContentModerationEmailDeliveryRepository)
	const workers = 32
	var winners atomic.Int64
	var failures atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			claim, err := deliveryRepo.ClaimLogEmailDelivery(ctx, log.ID)
			if err != nil {
				failures.Add(1)
				return
			}
			if claim.Claimed {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Zero(t, failures.Load())
	require.Equal(t, int64(1), winners.Load())

	require.NoError(t, deliveryRepo.CompleteLogEmailDelivery(ctx, log.ID, true))
	claim, err := deliveryRepo.ClaimLogEmailDelivery(ctx, log.ID)
	require.NoError(t, err)
	require.True(t, claim.Exists)
	require.False(t, claim.Claimed)
	require.Equal(t, "sent", claim.Status)

	var status string
	var sent bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
SELECT email_delivery_status, email_sent FROM content_moderation_logs WHERE id = $1`, log.ID).Scan(&status, &sent))
	require.Equal(t, "sent", status)
	require.True(t, sent)
}
