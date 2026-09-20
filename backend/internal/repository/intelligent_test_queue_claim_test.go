package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIntelligentClaimPickSQLStartsImmediately(t *testing.T) {
	t.Parallel()
	require.NotContains(t, intelligentClaimPickSQL, "<4")
	require.NotContains(t, intelligentClaimPickSQL, "status='running'")
	require.False(t, strings.Contains(strings.ToLower(intelligentClaimPickSQL), "not exists"))
	require.Contains(t, intelligentClaimPickSQL, "status='queued'")
}
