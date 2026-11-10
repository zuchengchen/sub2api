//go:build integration

package repository

import "testing"

func TestGatewayCacheLiveSidebandRouteRedisIntegration(t *testing.T) {
	assertLiveSidebandRoutePersistence(t, testRedis(t))
}
