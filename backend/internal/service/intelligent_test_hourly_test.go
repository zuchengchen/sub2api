package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type hourlyPelicanRepo struct {
	IntelligentTestRepository
	adminID    int64
	accountIDs []int64
	enqueued   []IntelligentTestEnqueue
	actors     []int64
}

func (r *hourlyPelicanRepo) Settings(context.Context) ([]IntelligentTestSetting, error) {
	return []IntelligentTestSetting{{TestType: "pelican", Enabled: true}}, nil
}
func (r *hourlyPelicanRepo) FirstAdminUserID(context.Context) (int64, error) { return r.adminID, nil }
func (r *hourlyPelicanRepo) ListGPTProOpenAIAccountIDs(context.Context) ([]int64, error) {
	return r.accountIDs, nil
}
func (r *hourlyPelicanRepo) Enqueue(_ context.Context, actor int64, req IntelligentTestEnqueue) (*IntelligentTestEnqueued, error) {
	r.actors = append(r.actors, actor)
	r.enqueued = append(r.enqueued, req)
	return &IntelligentTestEnqueued{}, nil
}
func (r *hourlyPelicanRepo) ClaimID(context.Context, int64) (*IntelligentTestRecord, error) {
	return nil, nil
}

func TestRunHourlyPelicanEnqueuesAstraLowOnGPTProAccounts(t *testing.T) {
	repo := &hourlyPelicanRepo{adminID: 3, accountIDs: []int64{11, 12}}
	svc := &IntelligentTestService{repo: repo}
	svc.runHourlyPelican(context.Background())
	require.Equal(t, []int64{3}, repo.actors)
	require.Len(t, repo.enqueued, 1)
	require.Equal(t, []int64{11, 12}, repo.enqueued[0].AccountIDs)
	require.Equal(t, []string{"pelican"}, repo.enqueued[0].TestTypes)
	require.Equal(t, "gpt-6-astra", repo.enqueued[0].Models["pelican"])
	require.Contains(t, repo.enqueued[0].IdempotencyKey, "pelican-hourly-")
}

func TestRunHourlyPelicanSkipsWithoutAdminOrAccounts(t *testing.T) {
	empty := &hourlyPelicanRepo{adminID: 0, accountIDs: []int64{1}}
	(&IntelligentTestService{repo: empty}).runHourlyPelican(context.Background())
	require.Empty(t, empty.enqueued)

	noAccounts := &hourlyPelicanRepo{adminID: 8}
	(&IntelligentTestService{repo: noAccounts}).runHourlyPelican(context.Background())
	require.Empty(t, noAccounts.enqueued)
}
