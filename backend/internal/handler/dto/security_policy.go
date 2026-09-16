package dto

import "github.com/Wei-Shaw/sub2api/internal/service"

type SecurityPolicyKeywordSeed struct {
	Keyword  string `json:"keyword"`
	Category string `json:"category"`
}

func SecurityPolicyKeywordSeedFromService(seed service.SecurityPolicyKeywordSeed) SecurityPolicyKeywordSeed {
	return SecurityPolicyKeywordSeed{Keyword: seed.Keyword, Category: seed.Category}
}

type SecurityPolicyKeyword struct {
	ID        int64  `json:"id"`
	GroupID   *int64 `json:"group_id,omitempty"`
	Keyword   string `json:"keyword"`
	Category  string `json:"category"`
	Enabled   bool   `json:"enabled"`
}

func SecurityPolicyKeywordsFromService(items []service.SecurityPolicyKeyword) []SecurityPolicyKeyword {
	out := make([]SecurityPolicyKeyword, 0, len(items))
	for i := range items {
		out = append(out, SecurityPolicyKeywordFromService(&items[i]))
	}
	return out
}

func SecurityPolicyKeywordFromService(item *service.SecurityPolicyKeyword) SecurityPolicyKeyword {
	if item == nil {
		return SecurityPolicyKeyword{}
	}
	return SecurityPolicyKeyword{
		ID: item.ID, GroupID: item.GroupID, Keyword: item.Keyword,
		Category: item.Category, Enabled: item.Enabled,
	}
}
