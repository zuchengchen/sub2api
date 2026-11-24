package adminaccess

import "testing"

func TestScopedPermissionCatalogRequiresExplicitGrant(t *testing.T) {
	for _, id := range []string{"accounts.identity.read", "accounts.options.write", "accounts.rate.write", "groups.list.read", "groups.capacity.read", "openai.tokens.refresh"} {
		found := false
		for _, entry := range PermissionCatalog() {
			if entry.ID == id {
				found = true
			}
		}
		if !found {
			t.Errorf("权限未登记: %s", id)
		}
	}
	if RouteAllowed(Policy{}, "POST", "/api/v1/admin/openai/refresh-token") {
		t.Fatal("旧策略不得继承新增权限")
	}
}

func TestScopedPermissionCatalogExcludesFullAccountUsageAndGroupSync(t *testing.T) {
	for _, id := range []string{"accounts.usage.read", "accounts.stats.read"} {
		policy := Policy{Rules: []Rule{{Permissions: []string{id}, AllGroups: true, AccountTypes: []string{"oauth"}}}}
		if ValidatePolicy(policy) == nil {
			t.Errorf("不应接受权限 %q", id)
		}
	}
	policy := Policy{Rules: []Rule{{Permissions: []string{"accounts.read", "accounts.update", "openai.quota.read"}, AllGroups: true, AccountTypes: []string{"oauth"}}}}
	for _, route := range []struct{ method, path string }{
		{"GET", "/accounts/:id/usage"},
		{"POST", "/accounts/usage/batch"},
		{"GET", "/accounts/:id/stats"},
		{"POST", "/accounts/upstream-billing-probe/group-preview"},
		{"POST", "/accounts/:id/upstream-billing-probe/group-sync"},
	} {
		if RouteAllowed(policy, route.method, "/api/v1/admin"+route.path) {
			t.Errorf("不应授权 %s %s", route.method, route.path)
		}
	}
}
