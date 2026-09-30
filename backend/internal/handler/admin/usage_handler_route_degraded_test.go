package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminUsageListRouteDegradedFilter(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  *bool
	}{
		{name: "absent", query: "", want: nil},
		{name: "true_http_fallback", query: "?route_degraded=true", want: boolPtr(true)},
		{name: "false_healthy_route", query: "?route_degraded=false", want: boolPtr(false)},
		{name: "trimmed", query: "?route_degraded=%201%20", want: boolPtr(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &adminUsageRepoCapture{}
			router := newAdminUsageRequestTypeTestRouter(repo)

			req := httptest.NewRequest(http.MethodGet, "/admin/usage"+tc.query, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, tc.want, repo.listFilters.RouteDegraded)
		})
	}
}

func TestAdminUsageListInvalidRouteDegradedFilter(t *testing.T) {
	repo := &adminUsageRepoCapture{}
	router := newAdminUsageRequestTypeTestRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/admin/usage?route_degraded=maybe", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "route_degraded")
}

// The usage page's stats cards follow the same route_degraded filter as the
// list, and the 30s stats cache keys on it.
func TestAdminUsageStatsHonorsRouteDegradedFilter(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  *bool
	}{
		{query: "", want: nil},
		{query: "&route_degraded=true", want: boolPtr(true)},
		{query: "&route_degraded=false", want: boolPtr(false)},
	} {
		repo := &adminUsageRepoCapture{}
		router := newAdminUsageRequestTypeTestRouter(repo)
		// A unique model keeps this request out of entries cached by other tests.
		req := httptest.NewRequest(http.MethodGet, "/admin/usage/stats?model=route-degraded-stats-probe"+tc.query, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "route-degraded-stats-probe", repo.statsFilters.Model, "each filter value misses the cache and reaches the repo")
		require.Equal(t, tc.want, repo.statsFilters.RouteDegraded)
	}

	repo := &adminUsageRepoCapture{}
	router := newAdminUsageRequestTypeTestRouter(repo)
	req := httptest.NewRequest(http.MethodGet, "/admin/usage/stats?route_degraded=maybe", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
