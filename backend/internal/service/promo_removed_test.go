//go:build unit

package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPromoCodeDomainConstantsRemoved(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../domain/constants.go"))
	require.NoError(t, err)
	require.NotContains(t, string(source), "PromoCodeStatus")
}

func TestPromoCodeEntSchemaRemoved(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	schemaDir := filepath.Join(filepath.Dir(file), "../../ent/schema")
	entries, err := os.ReadDir(schemaDir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.Contains(strings.ToLower(entry.Name()), "promo"), entry.Name())
	}
}

func TestPromoRemovalKeepsAffiliateInviteMigration(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	body, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../migrations/235_user_affiliate_invite_codes.sql"))
	require.NoError(t, err)
	require.Contains(t, string(body), "invite")
}
