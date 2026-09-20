package service

import (
	"context"
	"strings"
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

func TestRunScheduledPelicanEnqueuesRandomAnimalPrompt(t *testing.T) {
	repo := &hourlyPelicanRepo{adminID: 3, accountIDs: []int64{11, 12}}
	svc := &IntelligentTestService{repo: repo}
	svc.runScheduledPelican(context.Background())
	require.Equal(t, []int64{3}, repo.actors)
	require.Len(t, repo.enqueued, 1)
	require.Len(t, repo.enqueued[0].AccountIDs, 1)
	require.Contains(t, []int64{11, 12}, repo.enqueued[0].AccountIDs[0])
	require.Equal(t, []string{"pelican"}, repo.enqueued[0].TestTypes)
	require.Equal(t, "gpt-6-astra", repo.enqueued[0].Models["pelican"])
	prompt := repo.enqueued[0].Prompts["pelican"]
	require.Contains(t, prompt, "骑自行车的2D动画")
	require.Contains(t, prompt, "不要使用任何skill")
	matched := false
	for _, animal := range intelligentTestAnimals {
		if strings.Contains(prompt, "绘制一个"+animal+"骑自行车") {
			matched = true
			break
		}
	}
	require.True(t, matched, "prompt %q should name a known animal", prompt)
	require.Regexp(t, `^pelican-slot-\d{12}$`, repo.enqueued[0].IdempotencyKey)
}

type userPelicanListRepo struct {
	IntelligentTestRepository
	filter IntelligentTestFilter
}

func (r *userPelicanListRepo) UserPelicanTests(_ context.Context, f IntelligentTestFilter) (*UserPelicanTests, error) {
	r.filter = f
	return &UserPelicanTests{Items: []UserPelicanTest{}, Page: f.Page, PageSize: f.PageSize}, nil
}

func TestUserPelicanTestsListsLastHour(t *testing.T) {
	repo := &userPelicanListRepo{}
	svc := &IntelligentTestService{repo: repo}
	out, err := svc.UserPelicanTests(context.Background(), 9, IntelligentTestFilter{Page: 3, PageSize: 99})
	require.NoError(t, err)
	require.Equal(t, 1, repo.filter.Page)
	require.Equal(t, 12, repo.filter.PageSize)
	require.Equal(t, 12, out.PageSize)
}

func TestRunScheduledPelicanSkipsWithoutAdminOrAccounts(t *testing.T) {
	empty := &hourlyPelicanRepo{adminID: 0, accountIDs: []int64{1}}
	(&IntelligentTestService{repo: empty}).runScheduledPelican(context.Background())
	require.Empty(t, empty.enqueued)

	noAccounts := &hourlyPelicanRepo{adminID: 8}
	(&IntelligentTestService{repo: noAccounts}).runScheduledPelican(context.Background())
	require.Empty(t, noAccounts.enqueued)
}
