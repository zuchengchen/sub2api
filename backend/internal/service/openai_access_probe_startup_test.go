//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openAIAccessProbeStartupRepo struct {
	AccountRepository
	accounts []Account
	cursors  []int64
	messages []string
}

func (r *openAIAccessProbeStartupRepo) ListOpenAIOAuthRefreshErrors(_ context.Context, after int64, limit int, message string) ([]Account, error) {
	r.cursors = append(r.cursors, after)
	r.messages = append(r.messages, message)
	var page []Account
	for _, a := range r.accounts {
		if a.ID > after {
			page = append(page, a)
			if len(page) == limit {
				break
			}
		}
	}
	return page, nil
}

func TestOpenAIAccessProbeStartupRestoresOnlyCurrentRecoverableRows(t *testing.T) {
	base, _, _, _ := newOpenAIRecoveryFixture()
	cause := &openai.RefreshTokenError{Permanent: true}
	message := nonRetryableTokenRefreshMessage(cause)
	repo := &openAIAccessProbeStartupRepo{}
	for id := int64(1); id <= 65; id++ {
		a := snapshotOAuthRefreshAccount(base)
		a.ID = id
		a.Status = StatusError
		a.ErrorMessage = message
		repo.accounts = append(repo.accounts, *a)
	}
	// A repository page can go stale while the service is starting.
	repo.accounts[0].Credentials["expires_at"] = time.Now().Add(-time.Hour).Format(time.RFC3339)
	repo.accounts[1].Schedulable = false
	repo.accounts[2].ErrorMessage = "unrelated upstream error"
	repo.accounts[3].Status = StatusActive
	api := NewOAuthRefreshAPI(repo, nil)
	// Newer runtime evidence must win even if the persisted credential
	// fingerprint matches the one observed by the startup scan.
	current := &repo.accounts[4]
	newer := &openAIRefreshRejection{credentials: openAIRefreshFingerprint(current), err: &openai.RefreshTokenError{Permanent: true, StatusCode: 401}}
	api.openAIFailures.Store(current.ID, newer)

	api.restoreOpenAIAccessProbes(context.Background())

	require.Equal(t, []int64{0, 64}, repo.cursors)
	require.Equal(t, []string{message, message}, repo.messages)
	for i := range repo.accounts[:4] {
		require.Nil(t, api.openAIRejectionForAccount(&repo.accounts[i]))
	}
	require.Same(t, newer, api.openAIRejectionForAccount(current))
	for i := 5; i < len(repo.accounts); i++ {
		r := api.openAIRejectionForAccount(&repo.accounts[i])
		require.NotNil(t, r, "id %d", repo.accounts[i].ID)
		require.NotNil(t, r.probe)
		require.False(t, r.probe.next.IsZero())
		require.False(t, r.accessVerifiedAt(&repo.accounts[i], time.Now()))
	}
}
