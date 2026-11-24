package openai

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAICacheCreationTokensFromUsageNestedZeroWins(t *testing.T) {
	t.Parallel()

	usage := gjson.Parse(`{"input_tokens_details":{"cache_write_tokens":0},"cache_creation_input_tokens":19}`)
	total, ttl30m, ttl12h := CacheCreationTokensFromUsage(usage)
	require.Zero(t, total)
	require.Zero(t, ttl30m)
	require.Zero(t, ttl12h)
}
