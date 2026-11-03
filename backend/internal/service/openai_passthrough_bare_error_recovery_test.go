//go:build unit

package service

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIPassthroughBareErrorFollowedByCompleted(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done"} {
		for _, prefix := range []string{"", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"} {
			name := terminal + "/before_output"
			if prefix != "" {
				name = terminal + "/after_output"
			}
			t.Run(name, func(t *testing.T) {
				stream := prefix + "data: {\"type\":\"error\",\"error\":{\"code\":\"transient\",\"message\":\"retrying\"}}\n\n" +
					fmt.Sprintf("data: {\"type\":%q,\"response\":{\"id\":\"resp_recovered\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":4}}}\n\n", terminal)
				result, recorder, _, err := runPassthroughFlushTest(t, io.NopCloser(strings.NewReader(stream)), -1)
				require.NoError(t, err)
				require.Equal(t, 8, result.usage.InputTokens)
				require.Equal(t, 4, result.usage.OutputTokens)
				require.Contains(t, recorder.Body.String(), fmt.Sprintf("\"type\":%q", terminal))
				require.NotContains(t, recorder.Body.String(), "response.failed")
				require.NotContains(t, recorder.Body.String(), "retrying")
			})
		}
	}
}
