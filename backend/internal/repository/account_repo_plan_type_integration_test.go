//go:build integration

package repository

import (
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (s *AccountRepoSuite) TestListWithFiltersPlanTypeAliasesAndPagination() {
	for _, fixture := range []struct {
		name, platform string
		plan           any
	}{
		{"plus", service.PlatformOpenAI, " Plus "},
		{"pro100", service.PlatformOpenAI, "prolite"},
		{"pro100-alias", service.PlatformOpenAI, "PRO_100"},
		{"pro200", service.PlatformOpenAI, "pro"},
		{"pro200-legacy", service.PlatformOpenAI, " ChatGPT_Pro "},
		{"pro200-numeric", service.PlatformOpenAI, "pro-200"},
		{"pro500", service.PlatformOpenAI, "promax"},
		{"pro500-alias", service.PlatformOpenAI, " PRO_500\t"},
		{"free", service.PlatformOpenAI, "FREE"},
		{"grok-pro", service.PlatformGrok, "pro"},
		{"grok-free", service.PlatformGrok, "free"},
		{"claude-pro", service.PlatformAnthropic, "pro"},
		{"missing-plan", service.PlatformOpenAI, nil},
		{"empty-plan", service.PlatformOpenAI, ""},
		{"object-plan", service.PlatformOpenAI, map[string]any{"plan_type": "free"}},
		{"boolean-plan", service.PlatformOpenAI, true},
	} {
		mustCreateAccount(s.T(), s.client, &service.Account{
			Name: fixture.name, Platform: fixture.platform,
			Credentials: map[string]any{"plan_type": fixture.plan},
		})
	}
	deleted := mustCreateAccount(s.T(), s.client, &service.Account{
		Name: "deleted-pro", Platform: service.PlatformOpenAI, Credentials: map[string]any{"plan_type": "pro"},
	})
	s.Require().NoError(s.client.Account.UpdateOneID(deleted.ID).SetDeletedAt(time.Now()).Exec(s.ctx))

	for _, tc := range []struct {
		filter string
		want   []string
	}{
		{"plus", []string{"plus"}},
		{"prolite", []string{"pro100", "pro100-alias"}},
		{" Pro 100 ", []string{"pro100", "pro100-alias"}},
		{"pro", []string{"pro200", "pro200-legacy", "pro200-numeric"}},
		{"PRO_200", []string{"pro200", "pro200-legacy", "pro200-numeric"}},
		{"ChatGPT-Pro", []string{"pro200", "pro200-legacy", "pro200-numeric"}},
		{"promax", []string{"pro500", "pro500-alias"}},
		{"pro500", []string{"pro500", "pro500-alias"}},
		{"free", []string{"free"}},
		{"unknown", nil},
		{"true", nil},
		{"_-", nil},
		{"pro') OR 1=1 --", nil},
	} {
		s.Run(tc.filter, func() {
			// A one-row page catches filters applied after LIMIT or count.
			for page := 1; page <= len(tc.want)+1; page++ {
				accounts, result, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{
					Page: page, PageSize: 1, SortBy: "name", SortOrder: "asc",
				}, "", "", "", "", 0, "", tc.filter)
				s.Require().NoError(err)
				s.Require().Equal(int64(len(tc.want)), result.Total)
				if page > len(tc.want) {
					s.Require().Empty(accounts)
					continue
				}
				s.Require().Len(accounts, 1)
				s.Equal(tc.want[page-1], accounts[0].Name)
			}
			pool, err := s.repo.ListAllWithFilters(s.ctx, "", "", "", "", 0, "", tc.filter)
			s.Require().NoError(err)
			var names []string
			for _, account := range pool {
				names = append(names, account.Name)
			}
			s.ElementsMatch(tc.want, names, "the scheduler pool must use the same filter")
		})
	}

	for _, filter := range []string{"", " \t "} {
		accounts, result, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 100}, "", "", "", "", 0, "", filter)
		s.Require().NoError(err)
		s.Equal(int64(16), result.Total, "empty filters retain accounts without a known plan and other platforms")
		s.Len(accounts, 16)
	}
}

func (s *AccountRepoSuite) TestListWithFiltersPlanTypeCombinedFilters() {
	group := mustCreateGroup(s.T(), s.client, &service.Group{Name: "plan-filter-group", Platform: service.PlatformOpenAI})
	for i := 0; i < 5; i++ {
		account := mustCreateAccount(s.T(), s.client, &service.Account{
			Name: fmt.Sprintf("plan-%d", i), Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Credentials: map[string]any{"plan_type": "plus"}, Extra: map[string]any{"privacy_mode": service.PrivacyModeTrainingOff},
		})
		if i < 4 {
			_, err := s.client.AccountGroup.Create().SetAccountID(account.ID).SetGroupID(group.ID).Save(s.ctx)
			s.Require().NoError(err)
		}
		switch i {
		case 1:
			s.Require().NoError(s.client.Account.UpdateOneID(account.ID).SetExtra(map[string]any{"privacy_mode": service.PrivacyModeCFBlocked}).Exec(s.ctx))
		case 2:
			s.Require().NoError(s.client.Account.UpdateOneID(account.ID).SetStatus(service.StatusDisabled).Exec(s.ctx))
		case 3:
			s.Require().NoError(s.client.Account.UpdateOneID(account.ID).SetType(service.AccountTypeAPIKey).Exec(s.ctx))
		}
	}
	accounts, result, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10},
		service.PlatformOpenAI, service.AccountTypeOAuth, service.StatusActive, "plan", group.ID, service.PrivacyModeTrainingOff, "plus")
	s.Require().NoError(err)
	s.Require().Equal(int64(1), result.Total)
	s.Require().Len(accounts, 1)
	s.Equal("plan-0", accounts[0].Name)
	s.Contains(accounts[0].GroupIDs, group.ID)

	ungrouped, _, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10},
		"", "", "", "", service.AccountListGroupUngrouped, "", "plus")
	s.Require().NoError(err)
	s.Require().Len(ungrouped, 1)
	s.Equal("plan-4", ungrouped[0].Name)

	otherPlatform, result, err := s.repo.ListWithFilters(s.ctx, pagination.PaginationParams{Page: 1, PageSize: 10},
		service.PlatformGrok, "", "", "", 0, "", "plus")
	s.Require().NoError(err)
	s.Zero(result.Total)
	s.Empty(otherPlatform)
}
