package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise real animated bytes so converting a GIF into a still image or
// changing its frame timing cannot pass the response regression tests.
func openAIGIFTestBytes(t *testing.T) []byte {
	t.Helper()
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 32, 16), palette)
	second := image.NewPaletted(first.Bounds(), palette)
	second.SetColorIndex(0, 0, 1)
	var body bytes.Buffer
	require.NoError(t, gif.EncodeAll(&body, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{12, 12}, LoopCount: 0}))
	return body.Bytes()
}

func openAIGIFTestRequest(t *testing.T, encoding, endpoint, model string, fields map[string]any) ([]byte, string) {
	t.Helper()
	payload := map[string]any{"model": model, "prompt": "A circle moves in a loop", "output_format": "gif"}
	for key, value := range fields {
		payload[key] = value
	}
	if encoding == "json" {
		if endpoint == openAIImagesEditsEndpoint {
			payload["images"] = []map[string]string{{"image_url": "https://example.com/reference.png"}}
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		return body, "application/json"
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range payload {
		require.NoError(t, writer.WriteField(key, fmt.Sprint(value)))
	}
	part, err := writer.CreateFormFile("image", "reference.gif")
	require.NoError(t, err)
	_, err = part.Write(openAIGIFTestBytes(t))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return body.Bytes(), writer.FormDataContentType()
}

func TestOpenAIGIFForwardPreservesAnimation(t *testing.T) {
	for _, model := range []string{"gpt-image-2.6-flare", "gpt-image-2.6-sunburst"} {
		for _, request := range []struct{ encoding, endpoint string }{
			{"json", openAIImagesGenerationsEndpoint}, {"json", openAIImagesEditsEndpoint}, {"multipart", openAIImagesEditsEndpoint},
		} {
			for _, responseFormat := range []string{"b64_json", "url"} {
				t.Run(model+"/"+request.encoding+request.endpoint+"/"+responseFormat, func(t *testing.T) {
					body, contentType := openAIGIFTestRequest(t, request.encoding, request.endpoint, model, map[string]any{
						"frame_count": 2, "frame_duration_ms": 120, "response_format": responseFormat,
					})
					c, rec := newOpenAIImagesTestContext(t, body)
					c.Request.URL.Path = request.endpoint
					c.Request.Header.Set("Content-Type", contentType)
					gifBytes := openAIGIFTestBytes(t)
					encoded := base64.StdEncoding.EncodeToString(gifBytes)
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
						Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"data":[{"b64_json":%q}],"usage":{"input_tokens":10,"output_tokens":20}}`, encoded))),
					}}
					svc := newOpenAIImagesTestService(upstream)
					parsed, err := svc.ParseOpenAIImagesRequest(c, body)
					require.NoError(t, err)
					result, err := svc.ForwardImages(context.Background(), c, directImagesTestAccount(), body, parsed, "")
					require.NoError(t, err)
					require.Equal(t, 1, result.ImageCount, "count animations, not individual frames")
					require.Equal(t, []string{"32x16"}, result.ImageOutputSizes)
					require.Equal(t, "/backend-api/codex"+strings.TrimPrefix(request.endpoint, "/v1"), upstream.lastReq.URL.Path)
					require.Equal(t, model, gjson.GetBytes(upstream.lastBody, "model").String())
					require.EqualValues(t, 2, gjson.GetBytes(upstream.lastBody, "frame_count").Int())
					require.EqualValues(t, 120, gjson.GetBytes(upstream.lastBody, "frame_duration_ms").Int())
					require.False(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
					output := gjson.GetBytes(rec.Body.Bytes(), "data.0.b64_json").String()
					if responseFormat == "url" {
						url := gjson.GetBytes(rec.Body.Bytes(), "data.0.url").String()
						require.True(t, strings.HasPrefix(url, "data:image/gif;base64,"))
						output = strings.TrimPrefix(url, "data:image/gif;base64,")
					}
					decoded, err := base64.StdEncoding.DecodeString(output)
					require.NoError(t, err)
					require.Equal(t, gifBytes, decoded)
					animation, err := gif.DecodeAll(bytes.NewReader(decoded))
					require.NoError(t, err)
					require.Len(t, animation.Image, 2)
					require.Equal(t, []int{12, 12}, animation.Delay)
				})
			}
		}
	}
}

func TestOpenAIGIFOptionsValidation(t *testing.T) {
	for _, encoding := range []string{"json", "multipart"} {
		for _, tc := range []struct {
			name      string
			fields    map[string]any
			wantError string
		}{
			{"defaults", nil, ""},
			{"minimum", map[string]any{"frame_count": 2, "frame_duration_ms": 20}, ""},
			{"rounding_delegated", map[string]any{"frame_count": 16, "frame_duration_ms": 125}, ""},
			{"long_delay_preserved", map[string]any{"frame_duration_ms": 2001}, ""},
			{"count_too_small", map[string]any{"frame_count": 1}, "frame_count"},
			{"count_too_large", map[string]any{"frame_count": 17}, "frame_count"},
			{"count_fractional", map[string]any{"frame_count": 2.5}, "integer"},
			{"duration_fractional", map[string]any{"frame_duration_ms": 120.5}, "integer"},
			{"count_overflow", map[string]any{"frame_count": json.Number("999999999999999999999999")}, "integer"},
			{"duration_overflow", map[string]any{"frame_duration_ms": json.Number("999999999999999999999999")}, "integer"},
			{"count_boolean", map[string]any{"frame_count": true}, "integer"},
			{"duration_too_small", map[string]any{"frame_duration_ms": 19}, "frame_duration_ms"},
			{"requires_gif", map[string]any{"output_format": "png", "frame_count": 4}, "require output_format=gif"},
			{"stream", map[string]any{"stream": true}, "does not support"},
			{"partial_images", map[string]any{"partial_images": 1}, "does not support"},
			{"compression", map[string]any{"output_compression": 100}, "does not support"},
		} {
			t.Run(encoding+"/"+tc.name, func(t *testing.T) {
				body, contentType := openAIGIFTestRequest(t, encoding, openAIImagesEditsEndpoint, "gpt-image-2.6-flare", tc.fields)
				c, _ := newOpenAIImagesTestContext(t, body)
				c.Request.URL.Path = openAIImagesEditsEndpoint
				c.Request.Header.Set("Content-Type", contentType)
				parsed, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
				if tc.wantError != "" {
					require.ErrorContains(t, err, tc.wantError)
					return
				}
				require.NoError(t, err)
				for _, direct := range []bool{true, false} {
					var payload []byte
					prefix := ""
					if direct {
						payload, _, err = buildOpenAIImagesOAuthPayload(parsed, parsed.Model)
					} else {
						payload, err = buildOpenAIImagesResponsesRequest(parsed, parsed.Model)
						prefix = "tools.0."
					}
					require.NoError(t, err)
					for _, field := range []string{"frame_count", "frame_duration_ms"} {
						got := gjson.GetBytes(payload, prefix+field)
						want, exists := tc.fields[field]
						require.Equal(t, exists, got.Exists())
						if exists {
							require.Equal(t, fmt.Sprint(want), got.String())
						}
					}
				}
			})
		}
	}
	for _, raw := range []string{`"4"`, `[]`, `{}`} {
		body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"draw","output_format":"gif","frame_count":` + raw + `}`)
		c, _ := newOpenAIImagesTestContext(t, body)
		_, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
		require.ErrorContains(t, err, "integer")
	}
	body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"draw","output_format":"gif","frame_count":null,"frame_duration_ms":null,"mask":null}`)
	c, _ := newOpenAIImagesTestContext(t, body)
	parsed, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)
	require.Nil(t, parsed.FrameCount)
	require.Nil(t, parsed.FrameDurationMS)
}

func TestOpenAIGIFPreviewDenialDoesNotRetry(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2.6-flare","prompt":"draw","output_format":"gif","frame_count":4,"frame_duration_ms":120}`)
	errorBody := `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"output_format","message":"GIF output is not enabled for this request."}}`
	for _, account := range []*Account{directImagesTestAccount(), newOpenAIImagesAPIKeyAccount()} {
		t.Run(account.Type, func(t *testing.T) {
			c, rec := newOpenAIImagesTestContext(t, body)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(errorBody))}}
			svc := newOpenAIImagesTestService(upstream)
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			result, err := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
			require.Nil(t, result)
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, 400, rec.Code)
			require.JSONEq(t, errorBody, rec.Body.String())
			require.EqualValues(t, 4, gjson.GetBytes(upstream.lastBody, "frame_count").Int())
		})
	}
}

func TestOpenAIGIFMaskRejected(t *testing.T) {
	body, contentType := openAIGIFTestRequest(t, "json", openAIImagesEditsEndpoint, "gpt-image-2.6-flare", map[string]any{"mask": map[string]string{"image_url": "https://example.com/mask.png"}})
	c, _ := newOpenAIImagesTestContext(t, body)
	c.Request.URL.Path = openAIImagesEditsEndpoint
	c.Request.Header.Set("Content-Type", contentType)
	_, err := newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, body)
	require.ErrorContains(t, err, "masks")

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for name, value := range map[string]string{"model": "gpt-image-2.6-flare", "prompt": "draw", "output_format": "gif"} {
		require.NoError(t, writer.WriteField(name, value))
	}
	for _, name := range []string{"image", "mask"} {
		part, err := writer.CreateFormFile(name, "input.png")
		require.NoError(t, err)
		_, err = part.Write([]byte("fixture"))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	c, _ = newOpenAIImagesTestContext(t, buf.Bytes())
	c.Request.URL.Path = openAIImagesEditsEndpoint
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	_, err = newOpenAIImagesTestService(nil).ParseOpenAIImagesRequest(c, buf.Bytes())
	require.ErrorContains(t, err, "masks")
}

func TestOpenAIGIFAPIKeyPassthrough(t *testing.T) {
	for _, encoding := range []string{"json", "multipart"} {
		t.Run(encoding, func(t *testing.T) {
			body, contentType := openAIGIFTestRequest(t, encoding, openAIImagesEditsEndpoint, "gpt-image-2.6-sunburst", map[string]any{"frame_count": 4, "frame_duration_ms": 120})
			c, rec := newOpenAIImagesTestContext(t, body)
			c.Request.URL.Path = openAIImagesEditsEndpoint
			c.Request.Header.Set("Content-Type", contentType)
			encoded := base64.StdEncoding.EncodeToString(openAIGIFTestBytes(t))
			response := fmt.Sprintf(`{"data":[{"b64_json":%q}],"output_format":"gif"}`, encoded)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(response))}}
			svc := newOpenAIImagesTestService(upstream)
			parsed, err := svc.ParseOpenAIImagesRequest(c, body)
			require.NoError(t, err)
			_, err = svc.ForwardImages(context.Background(), c, newOpenAIImagesAPIKeyAccount(), body, parsed, "")
			require.NoError(t, err)
			if encoding == "json" {
				require.EqualValues(t, 4, gjson.GetBytes(upstream.lastBody, "frame_count").Int())
				require.EqualValues(t, 120, gjson.GetBytes(upstream.lastBody, "frame_duration_ms").Int())
			} else {
				require.NoError(t, upstream.lastReq.ParseMultipartForm(1024*1024))
				defer upstream.lastReq.MultipartForm.RemoveAll()
				require.Equal(t, "4", upstream.lastReq.FormValue("frame_count"))
				require.Equal(t, "120", upstream.lastReq.FormValue("frame_duration_ms"))
			}
			require.Equal(t, encoded, gjson.GetBytes(rec.Body.Bytes(), "data.0.b64_json").String())
		})
	}
}
