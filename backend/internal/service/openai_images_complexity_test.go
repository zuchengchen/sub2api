package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIImagesBareAliasesForwardToSunburst(t *testing.T) {
	for _, alias := range []string{"gpt-image-2.5", "gpt-image-2.6"} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey} {
			t.Run(alias+"/"+accountType, func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":%q,"prompt":"A blue circle on a white background.","n":1,"response_format":"b64_json"}`, alias))
				c, rec := newOpenAIImagesTestContext(t, body)
				upstream := &httpUpstreamRecorder{resp: openAIImagesJSONResponse()}
				svc := newOpenAIImagesTestService(upstream)
				parsed, err := svc.ParseOpenAIImagesRequest(c, body)
				require.NoError(t, err)
				account := directImagesTestAccount()
				account.Type = accountType
				account.Credentials["api_key"] = "test-key"
				result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
				require.NoError(t, err)
				wantPath := "/backend-api/codex/images/generations"
				if accountType == AccountTypeAPIKey {
					wantPath = openAIImagesGenerationsEndpoint
				}
				require.Equal(t, wantPath, upstream.lastReq.URL.Path)
				require.Equal(t, alias, result.Model, "retain the client's requested name")
				require.Equal(t, alias+"-sunburst", result.UpstreamModel, "record the concrete model sent upstream")
				require.Empty(t, result.UpstreamResponseModel, "never invent an upstream-reported model")
				require.Equal(t, alias+"-sunburst", gjson.GetBytes(upstream.lastBody, "model").String())
				for _, key := range []string{"complexity", "quality", "reasoning", "tools"} {
					require.False(t, gjson.GetBytes(upstream.lastBody, key).Exists(), key)
				}
				require.Equal(t, "aGVsbG8=", gjson.GetBytes(rec.Body.Bytes(), "data.0.b64_json").String())
			})
		}
	}
}

func TestOpenAIImagesComplexityForwarding(t *testing.T) {
	for _, encoding := range []string{"json-generation", "json-edit", "multipart-edit"} {
		for _, complexity := range []string{"", "low", "medium", "high"} {
			for _, fallback := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/fallback=%t", encoding, complexity, fallback), func(t *testing.T) {
					fields := map[string]any{"model": "gpt-image-2.5-flare", "prompt": "A blue circle", "quality": "medium"}
					if complexity != "" {
						fields["complexity"] = complexity
					}
					endpoint := openAIImagesGenerationsEndpoint
					if encoding != "json-generation" {
						endpoint = openAIImagesEditsEndpoint
						fields["images"] = []map[string]string{{"image_url": "data:image/png;base64,AA=="}}
					}
					body, err := json.Marshal(fields)
					require.NoError(t, err)
					contentType := "application/json"
					if encoding == "multipart-edit" {
						var buf bytes.Buffer
						writer := multipart.NewWriter(&buf)
						for key, value := range fields {
							if key != "images" {
								require.NoError(t, writer.WriteField(key, fmt.Sprint(value)))
							}
						}
						part, err := writer.CreateFormFile("image", "reference.png")
						require.NoError(t, err)
						_, err = part.Write([]byte("image-content"))
						require.NoError(t, err)
						require.NoError(t, writer.Close())
						body, contentType = buf.Bytes(), writer.FormDataContentType()
					}
					c, _ := newOpenAIImagesTestContext(t, body)
					c.Request.URL.Path = endpoint
					c.Request.Header.Set("Content-Type", contentType)
					calls := 0
					upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
						calls++
						payload, err := io.ReadAll(req.Body)
						require.NoError(t, err)
						prefix := ""
						if calls == 1 {
							require.Equal(t, "/backend-api/codex"+strings.TrimPrefix(endpoint, "/v1"), req.URL.Path)
						} else {
							require.Equal(t, "/backend-api/codex/responses", req.URL.Path)
							prefix = "tools.0."
						}
						require.Equal(t, "gpt-image-2.5-flare", gjson.GetBytes(payload, prefix+"model").String())
						require.Equal(t, "medium", gjson.GetBytes(payload, prefix+"quality").String())
						value := gjson.GetBytes(payload, prefix+"complexity")
						require.Equal(t, complexity != "", value.Exists(), "omitted complexity must remain omitted")
						require.Equal(t, complexity, value.String(), "complexity is independent of quality and driver reasoning")
						if fallback && calls == 1 {
							return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"endpoint unavailable"}}`))}, nil
						}
						if fallback {
							return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"image_generation_call\",\"result\":\"aGVsbG8=\"}]}}\n\n"))}, nil
						}
						return openAIImagesJSONResponse(), nil
					}}
					svc := newOpenAIImagesTestService(upstream)
					parsed, err := svc.ParseOpenAIImagesRequest(c, body)
					require.NoError(t, err)
					_, err = svc.ForwardImages(context.Background(), c, directImagesTestAccount(), body, parsed, "")
					require.NoError(t, err)
					wantCalls := 1
					if fallback {
						wantCalls = 2
					}
					require.Equal(t, wantCalls, calls)
				})
			}
		}
	}
}

func TestOpenAIImagesComplexityRequiresNativeCapability(t *testing.T) {
	body := []byte(`{"prompt":"A blue circle","complexity":"high"}`)
	c, _ := newOpenAIImagesTestContext(t, body)
	parsed, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	require.Equal(t, OpenAIImagesCapabilityNative, parsed.RequiredCapability,
		"explicit complexity must not be routed to a basic image account that ignores it")
}

func TestOpenAIImagesComplexityNullUsesUpstreamDefault(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2.5-flare","prompt":"A blue circle","complexity":null}`)
	c, _ := newOpenAIImagesTestContext(t, body)
	parsed, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	payload, _, err := buildOpenAIImagesOAuthPayload(parsed, parsed.Model)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(payload, "complexity").Exists())
}
