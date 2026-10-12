package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration268IntelligentTestTibo(t *testing.T) {
	body, err := FS.ReadFile("268_intelligent_test_tibo.sql")
	require.NoError(t, err)
	sql := string(body)
	require.Contains(t, sql, "test_type")
	require.Contains(t, sql, "'tibo'")
	require.Contains(t, sql, `"evaluator":"tibo_answer"`)
	require.Contains(t, sql, `"expected_answer":"True"`)
	require.Contains(t, sql, "Tell me whether you know who Tibo is in OpenAI based on your knowledge without searching online. Only return True or  False.")
	require.Contains(t, sql, "user_visible")
	require.Contains(t, sql, "ON CONFLICT(test_type) DO NOTHING")
}
