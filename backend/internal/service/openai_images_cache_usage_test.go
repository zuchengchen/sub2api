package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIImagesReportedCacheUsage(t *testing.T) {
	for _, tt := range []struct {
		name, details string
		total, image  int
	}{
		{"absent", `{"image_tokens":8}`, 0, 0},
		{"aggregate only", `{"image_tokens":8,"cached_tokens":7}`, 7, 0},
		{"explicit zero", `{"image_tokens":8,"cached_tokens":0,"cached_tokens_details":{"image_tokens":8,"text_tokens":2}}`, 0, 0},
		{"aggregate wins", `{"image_tokens":8,"cached_tokens":3,"cached_tokens_details":{"image_tokens":7,"text_tokens":2}}`, 3, 3},
		{"aggregate bounded by input", `{"image_tokens":8,"cached_tokens":15,"cached_tokens_details":{"image_tokens":9}}`, 10, 8},
		{"text and image parts", `{"image_tokens":8,"cached_tokens_details":{"image_tokens":5,"text_tokens":2}}`, 7, 5},
		{"part exceeds own input", `{"image_tokens":2,"text_tokens":4,"cached_tokens_details":{"image_tokens":11,"text_tokens":9}}`, 6, 2},
		{"no text input breakdown", `{"image_tokens":8,"cached_tokens_details":{"text_tokens":20}}`, 2, 0},
		{"no image input breakdown", `{"cached_tokens_details":{"image_tokens":8,"text_tokens":3}}`, 3, 0},
		{"fractional part ignored", `{"image_tokens":8,"cached_tokens_details":{"image_tokens":1.5,"text_tokens":2}}`, 2, 0},
		{"invalid aggregate", `{"image_tokens":8,"cached_tokens":"7","cached_tokens_details":{"image_tokens":4}}`, 0, 0},
		{"hostile aggregate", `{"image_tokens":8,"cached_tokens":1e999999999,"cached_tokens_details":{"image_tokens":4}}`, 0, 0},
		{"parts overflow", `{"image_tokens":8,"cached_tokens_details":{"image_tokens":9223372036854775808,"text_tokens":-1}}`, 0, 0},
		{"sum bounded without overflow", `{"image_tokens":8,"cached_tokens_details":{"image_tokens":9223372036854775807,"text_tokens":9223372036854775807}}`, 10, 8},
	} {
		t.Run(tt.name, func(t *testing.T) {
			meter := `{"input_tokens":10,"output_tokens":4,"output_tokens_details":{"image_tokens":4},"input_tokens_details":` + tt.details + "}"
			body := []byte(`{"usage":` + meter + "}")
			direct, ok := codexDirectImagesUsage(body)
			require.True(t, ok)
			tool, ok := openAIImagesToolUsageFromGJSON(gjson.Parse(meter))
			require.True(t, ok, "malformed optional cache fields must not replace the image meter with main model usage")
			public, _ := extractOpenAIImagesUsageFromJSONBytes(body, true)
			for _, got := range []OpenAIUsage{direct, tool, public} {
				require.Equal(t, tt.total, got.CacheReadInputTokens)
				require.Equal(t, tt.image, got.ImageCacheReadTokens)
				require.Equal(t, 10, got.InputTokens)
				require.Equal(t, 4, got.ImageOutputTokens)
			}
		})
	}
}

func TestOpenAIImagesCacheSnapshotsAndOwners(t *testing.T) {
	const withParts = `{"usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"image_tokens":6,"cached_tokens":7,"cached_tokens_details":{"image_tokens":4}}}}`
	for _, tt := range []struct {
		name, last   string
		total, image int
	}{
		{"duplicate", withParts, 7, 4},
		{"missing report", `{"usage":{"input_tokens":10,"output_tokens":6}}`, 7, 4},
		{"invalid report", `{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":1e999999999}}}`, 7, 4},
		{"explicit aggregate only", `{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":2}}}`, 2, 0},
		{"explicit zero", `{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":0}}}`, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var usage OpenAIUsage
			mergeOpenAIUsage(&usage, []byte(withParts), true)
			mergeOpenAIUsage(&usage, []byte(tt.last), true)
			require.Equal(t, tt.total, usage.CacheReadInputTokens)
			require.Equal(t, tt.image, usage.ImageCacheReadTokens)
		})
	}
	// Select the same nested object as the generic usage parser. The adapter's
	// existing aggregate alias also takes precedence over details-only reports.
	aliased, _ := extractOpenAIImagesUsageFromJSONBytes([]byte(`{"usage":null,"data":{"response":{"usage":{"input_tokens":10,"cache_read_input_tokens":2,"input_tokens_details":{"image_tokens":8,"cached_tokens_details":{"image_tokens":7}}}}}}`), true)
	require.Equal(t, 2, aliased.CacheReadInputTokens)
	require.Equal(t, 2, aliased.ImageCacheReadTokens)

	const model = `{"input_tokens":100,"output_tokens":9,"input_tokens_details":{"cached_tokens":3}}`
	const tool = `{"input_tokens":10,"output_tokens":4,"output_tokens_details":{"image_tokens":4},"input_tokens_details":{"image_tokens":8,"cached_tokens":7,"cached_tokens_details":{"image_tokens":5}}}`
	body := []byte(`{"type":"response.completed","response":{"usage":` + model + `,"tool_usage":{"image_gen":` + tool + `}}}`)
	main, ok := extractOpenAIUsageFromJSONBytes(body)
	require.True(t, ok)
	require.Equal(t, 100, main.InputTokens)
	require.Equal(t, 3, main.CacheReadInputTokens)
	require.Zero(t, main.ImageCacheReadTokens, "the hosted tool's cached images are separate from the main model's aggregate")
	svc := &OpenAIGatewayService{}
	var bridged OpenAIUsage
	svc.parseOpenAIImagesSSEUsageBytes(body, &bridged)
	require.Equal(t, 10, bridged.InputTokens)
	require.Equal(t, 7, bridged.CacheReadInputTokens)
	require.Equal(t, 5, bridged.ImageCacheReadTokens)
	svc.parseOpenAIImagesSSEUsageBytes([]byte(`{"type":"response.completed","response":{"usage":`+model+`,"tool_usage":{"image_gen":{"images":1}}}}`), &bridged)
	require.Equal(t, main.CacheReadInputTokens, bridged.CacheReadInputTokens)
	require.Zero(t, bridged.ImageCacheReadTokens)
}

func TestForwardImagesReportedCacheUsage(t *testing.T) {
	mapped := newOpenAIImagesAPIKeyAccount()
	mapped.Credentials["model_mapping"] = map[string]any{"gpt-image-2": "gemini-2.5-flash-image"}
	for _, route := range []struct {
		name, model string
		account     *Account
	}{
		{"api_key", "gpt-image-2", newOpenAIImagesAPIKeyAccount()},
		{"codex_direct", "gpt-image-2", directImagesTestAccount()},
		{"responses_tool", "gpt-image-1", directImagesTestAccount()},
		{"mapped_non_gpt", "gpt-image-2", mapped},
	} {
		for _, operation := range []string{"generations", "edits"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", route.name, operation, stream), func(t *testing.T) {
					inputImage, imageTokens := "", 0
					details := `{"cached_tokens_details":{"text_tokens":3}}`
					if operation == "edits" {
						inputImage = `,"images":[{"image_url":"data:image/png;base64,AA=="}]`
						imageTokens = 4
						details = `{"image_tokens":6,"cached_tokens_details":{"text_tokens":3,"image_tokens":4}}`
					}
					meter := `{"input_tokens":10,"output_tokens":4,"output_tokens_details":{"image_tokens":4},"input_tokens_details":` + details + "}"
					payload := `{"data":[{"b64_json":"AA=="}],"usage":` + meter + "}"
					contentType := "application/json"
					clientPath := "/v1/images/" + operation
					wantUpstream := clientPath
					if route.name == "responses_tool" {
						wantUpstream = "/backend-api/codex/responses"
						contentType = "text/event-stream"
						payload = `data: {"type":"response.completed","response":{"created_at":1710000000,"usage":{"input_tokens":100,"output_tokens":9,"input_tokens_details":{"cached_tokens":61}},"tool_usage":{"image_gen":` + meter + `},"output":[{"type":"image_generation_call","id":"ig_test","result":"AA=="}]}}` + "\n\ndata: [DONE]\n\n"
					} else {
						if route.name == "codex_direct" {
							wantUpstream = "/backend-api/codex/images/" + operation
						}
						if stream {
							contentType = "text/event-stream"
							payload = `data: {"type":"image_generation.completed","b64_json":"AA==","usage":` + meter + "}\n\ndata: [DONE]\n\n"
						}
					}
					request := []byte(fmt.Sprintf(`{"model":%q,"prompt":"draw","stream":%t%s}`, route.model, stream, inputImage))
					c, _ := newOpenAIImagesTestContext(t, request)
					c.Request.URL.Path = clientPath
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
						Body: io.NopCloser(strings.NewReader(payload)),
					}}
					svc := newOpenAIImagesTestService(upstream)
					parsed, err := svc.ParseOpenAIImagesRequest(c, request)
					require.NoError(t, err)
					result, err := svc.ForwardImages(context.Background(), c, route.account, request, parsed, "")
					require.NoError(t, err)
					require.Equal(t, wantUpstream, upstream.lastReq.URL.Path)
					require.Equal(t, 1, result.ImageCount)
					require.Equal(t, 10, result.Usage.InputTokens)
					require.Equal(t, 4, result.Usage.ImageOutputTokens)
					if route.name == "mapped_non_gpt" {
						require.Equal(t, "gemini-2.5-flash-image", result.UpstreamModel)
						require.Zero(t, result.Usage.CacheReadInputTokens)
						require.Zero(t, result.Usage.ImageCacheReadTokens)
						return
					}
					require.Equal(t, 3+imageTokens, result.Usage.CacheReadInputTokens)
					require.Equal(t, imageTokens, result.Usage.ImageCacheReadTokens)
				})
			}
		}
	}
}
