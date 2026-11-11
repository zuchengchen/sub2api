//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func requireImageOutputDimensions(t *testing.T, encoded string, width, height int) {
	t.Helper()
	if strings.HasPrefix(encoded, "data:") {
		_, encoded, _ = strings.Cut(encoded, ",")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	img, _, err := image.Decode(bytes.NewReader(raw))
	require.NoError(t, err)
	require.Equal(t, image.Rect(0, 0, width, height), img.Bounds())
}

func TestOpenAIImageOutputPreservesContentAndDisclosesResize(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.NRGBA{R: 255, A: 255})
		}
	}
	var raw bytes.Buffer
	require.NoError(t, png.Encode(&raw, img))
	encoded := base64.StdEncoding.EncodeToString(raw.Bytes())
	c, _ := newOpenAIImagesTestContext(t, nil)
	setOpenAIImageOutputRequest(c, []byte(`{"model":"gpt-image-2.6-flare","size":"32x32","output_format":"png"}`))
	upstream := []byte(fmt.Sprintf(`{"data":[{"model":"gpt-image-2-codex","b64_json":%q}],"usage":{"input_tokens":5,"output_tokens":41,"total_tokens":46}}`, encoded))
	result := prepareOpenAIImageOutput(c, upstream)
	item := gjson.GetBytes(result, "data.0")
	requireImageOutputDimensions(t, item.Get("b64_json").String(), 32, 32)
	require.Equal(t, "gpt-image-2-codex", item.Get("model").String())
	require.Equal(t, "gpt-image-2.6-flare", item.Get("sub2api_image.requested_model").String())
	require.Equal(t, "different", item.Get("sub2api_image.model_status").String())
	require.Equal(t, "64x32", item.Get("sub2api_image.upstream_size").String())
	require.Equal(t, "32x32", item.Get("sub2api_image.output_size").String())
	require.True(t, item.Get("sub2api_image.postprocessed").Bool())
	require.EqualValues(t, 41, gjson.GetBytes(result, "usage.output_tokens").Int())
	require.Equal(t, encoded, gjson.GetBytes(upstream, "data.0.b64_json").String(), "upstream accounting input stays unchanged")
	output, err := base64.StdEncoding.DecodeString(item.Get("b64_json").String())
	require.NoError(t, err)
	decoded, err := png.Decode(bytes.NewReader(output))
	require.NoError(t, err)
	require.Equal(t, color.NRGBA{R: 255, A: 255}, color.NRGBAModel.Convert(decoded.At(16, 16)))
	require.Equal(t, color.NRGBA{}, color.NRGBAModel.Convert(decoded.At(16, 0)), "contain padding, no stretching or crop")
}

func TestOpenAIImageOutputAnimationUnknownModelAndTurnReset(t *testing.T) {
	c, _ := newOpenAIImagesTestContext(t, nil)
	setOpenAIImageOutputRequest(c, []byte(`{"model":"gpt-image-2.6-flare","size":"64x64","output_format":"gif"}`))
	encoded := base64.StdEncoding.EncodeToString(openAIGIFTestBytes(t))
	body := []byte(fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, encoded))
	result := prepareOpenAIImageOutput(c, body)
	require.Equal(t, encoded, gjson.GetBytes(result, "data.0.b64_json").String(), "animated bytes, frame count and timing must stay intact")
	require.False(t, gjson.GetBytes(result, "data.0.model").Exists())
	require.Equal(t, "unreported", gjson.GetBytes(result, "data.0.sub2api_image.model_status").String())
	require.False(t, gjson.GetBytes(result, "data.0.sub2api_image.postprocessed").Bool())
	require.Contains(t, gjson.GetBytes(result, "data.0.sub2api_image.warnings").String(), "image_resize_unavailable")

	setOpenAIImageOutputRequest(c, []byte(`{"model":"gpt-6.1-sol","input":"hello"}`))
	require.Equal(t, body, prepareOpenAIImageOutput(c, body), "a new non-image turn cannot inherit previous image settings")
	setOpenAIImageOutputRequest(c, []byte(`{"model":"gpt-6.1-sol","input":[{"type":"additional_tools","tools":[{"type":"image_generation","size":"16x16"}]}]}`))
	pngBody := []byte(fmt.Sprintf(`{"output":[{"type":"image_generation_call","result":%q}]}`, encodeOpenAIImageTestPNG(t, 32, 32)))
	requireImageOutputDimensions(t, gjson.GetBytes(prepareOpenAIImageOutput(c, pngBody), "output.0.result").String(), 16, 16)
}

func TestOpenAIImageOutputFormatsAndSizeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, size, format, encoded string
		resized                     bool
	}{
		{"jpeg", "32x32", "jpeg", encodeOpenAIImageTestJPEG(t, 64, 64), true},
		{"exact_png", "32x32", "png", encodeOpenAIImageTestPNG(t, 32, 32), false},
		{"auto", "auto", "png", encodeOpenAIImageTestPNG(t, 32, 32), false},
		{"oversized_target", "999999x999999", "png", encodeOpenAIImageTestPNG(t, 32, 32), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newOpenAIImagesTestContext(t, nil)
			setOpenAIImageOutputRequest(c, []byte(fmt.Sprintf(`{"model":"gpt-image-2","size":%q,"output_format":%q}`, tc.size, tc.format)))
			body := []byte(fmt.Sprintf(`{"type":"image_generation.completed","url":"data:image/%s;base64,%s"}`, tc.format, tc.encoded))
			out := prepareOpenAIImageOutput(c, body)
			requireImageOutputDimensions(t, gjson.GetBytes(out, "url").String(), 32, 32)
			require.Equal(t, tc.resized, gjson.GetBytes(out, "sub2api_image.postprocessed").Bool())
			require.Equal(t, tc.format, gjson.GetBytes(out, "output_format").String())
			if !tc.resized {
				require.Equal(t, gjson.GetBytes(body, "url").String(), gjson.GetBytes(out, "url").String())
			}
		})
	}
}

func TestOpenAIImageOutputDoesNotMislabelPNGAsGIF(t *testing.T) {
	c, _ := newOpenAIImagesTestContext(t, nil)
	setOpenAIImageOutputRequest(c, []byte(`{"model":"gpt-image-2.6-flare","output_format":"gif"}`))
	encoded := encodeOpenAIImageTestPNG(t, 32, 32)
	body := []byte(fmt.Sprintf(`{"output_format":"gif","data":[{"url":"data:image/gif;base64,%s"}]}`, encoded))
	out := prepareOpenAIImageOutput(c, body)
	require.Equal(t, "png", gjson.GetBytes(out, "output_format").String())
	require.Equal(t, "data:image/png;base64,"+encoded, gjson.GetBytes(out, "data.0.url").String())
	require.Contains(t, gjson.GetBytes(out, "data.0.sub2api_image.warnings").String(), "upstream_image_format_differs")
	require.False(t, gjson.GetBytes(out, "data.0.sub2api_image.postprocessed").Bool())
}

func TestOpenAIImageOutputImagesRoutes(t *testing.T) {
	for _, route := range []string{"direct", "responses", "api_key"} {
		for _, stream := range []bool{false, true} {
			for _, responseFormat := range []string{"b64_json", "url"} {
				t.Run(fmt.Sprintf("%s/%t/%s", route, stream, responseFormat), func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"model":"gpt-image-2.6-flare","prompt":"draw","size":"1024x1024","output_format":"png","stream":%t,"response_format":%q}`, stream, responseFormat))
					c, rec := newOpenAIImagesTestContext(t, body)
					encoded := encodeOpenAIImageTestPNG(t, 1254, 1254)
					contentType := "application/json"
					response := fmt.Sprintf(`{"model":"gpt-image-2-codex","data":[{"b64_json":%q}],"usage":{"input_tokens":5,"output_tokens":41,"total_tokens":46}}`, encoded)
					if stream {
						contentType = "text/event-stream"
						response = fmt.Sprintf("event: image_generation.completed\nid: keep-id\ndata: {\"type\":\"image_generation.completed\",\"model\":\"gpt-image-2-codex\",\ndata: \"b64_json\":%q,\"usage\":{\"output_tokens\":41}}\n\n", encoded)
					}
					if route == "responses" {
						contentType = "text/event-stream"
						response = fmt.Sprintf("data: {\"type\":\"response.created\",\"response\":{\"tools\":[{\"type\":\"function\",\"name\":\"other\"},{\"type\":\"image_generation\",\"model\":\"gpt-image-2-codex\"}]}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"image_generation_call\",\"result\":%q}],\"tool_usage\":{\"image_gen\":{\"output_tokens\":41}}}}\n\n", encoded)
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response))}}
					svc := newOpenAIImagesTestService(upstream)
					parsed, err := svc.ParseOpenAIImagesRequest(c, body)
					require.NoError(t, err)
					account := directImagesTestAccount()
					ctx := context.Background()
					if route == "responses" {
						ctx = withOpenAIImagesForceResponses(ctx)
					}
					if route == "api_key" {
						account.Type = AccountTypeAPIKey
						account.Credentials = map[string]any{"api_key": "test-key"}
					}
					result, err := svc.ForwardImages(ctx, c, account, body, parsed, "")
					require.NoError(t, err)
					require.Equal(t, 1, result.ImageCount)
					require.Equal(t, []string{"1254x1254"}, result.ImageOutputSizes, "billing retains upstream dimensions")
					item := gjson.GetBytes(rec.Body.Bytes(), "data.0")
					if stream {
						event, ok := findOpenAIImageTestSSEEvent(parseOpenAIImageTestSSEEvents(rec.Body.String()), "image_generation.completed")
						require.True(t, ok)
						item = gjson.Parse(event.Data)
					}
					value := item.Get("b64_json").String()
					if value == "" {
						value = item.Get("url").String()
					}
					requireImageOutputDimensions(t, value, 1024, 1024)
					if url := item.Get("url").String(); url != "" {
						requireImageOutputDimensions(t, url, 1024, 1024)
					}
					require.Equal(t, "1254x1254", item.Get("sub2api_image.upstream_size").String())
					require.Equal(t, "different", item.Get("sub2api_image.model_status").String())
					require.Equal(t, "gpt-image-2-codex", item.Get("sub2api_image.reported_model").String())
					if route == "api_key" && stream {
						require.Contains(t, rec.Body.String(), "id: keep-id")
					}
				})
			}
		}
	}
}

func TestOpenAIImageOutputResponsesHTTPAndWS(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", transport, stream), func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				cfg := &config.Config{}
				cfg.Gateway.OpenAIWS.Enabled = transport == "ws"
				cfg.Gateway.OpenAIWS.OAuthEnabled = true
				cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
				cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
				cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
				encoded := encodeOpenAIImageTestPNG(t, 1254, 1254)
				created := []byte(`{"type":"response.created","response":{"id":"resp_image","model":"gpt-6.1-sol","tools":[{"type":"function","name":"other"},{"type":"image_generation","model":"gpt-image-2-codex"}]}}`)
				completed := []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_image","model":"gpt-6.1-sol","status":"completed","output":[{"type":"image_generation_call","id":"ig_1","result":%q}],"usage":{"input_tokens":5,"output_tokens":41,"total_tokens":46}}}`, encoded))
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf("data: %s\n\ndata: %s\n\n", created, completed)))}}
				conn := &openAIWSCaptureConn{events: [][]byte{created, completed}}
				pool := newOpenAIWSConnPool(cfg)
				pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: conn})
				defer pool.Close()
				svc := &OpenAIGatewayService{tiboRouteDisabled: true, cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
				account := &Account{ID: 85, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "test-token"}, Extra: astraHealthyTestExtra(map[string]any{"responses_websockets_v2_enabled": transport == "ws"})}
				request := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","input":"draw","stream":%t,"tools":[{"type":"image_generation","model":"gpt-image-2.6-flare","size":"1024x1024","output_format":"png"}]}`, stream))
				result, err := svc.Forward(context.Background(), c, account, request)
				require.NoError(t, err)
				require.Equal(t, []string{"1254x1254"}, result.ImageOutputSizes)
				require.Equal(t, 41, result.Usage.OutputTokens)
				outbound := upstream.lastBody
				if transport == "ws" {
					outbound, err = json.Marshal(conn.writes[0])
					require.NoError(t, err)
				}
				require.Equal(t, "gpt-image-2.6-flare", gjson.GetBytes(outbound, "tools.0.model").String())
				require.Equal(t, "1024x1024", gjson.GetBytes(outbound, "tools.0.size").String())
				output := gjson.GetBytes(rec.Body.Bytes(), "output.0")
				if stream {
					forEachOpenAISSEDataPayload(rec.Body.String(), func(data []byte) {
						if gjson.GetBytes(data, "type").String() == "response.completed" {
							output = gjson.GetBytes(data, "response.output.0")
						}
					})
				}
				requireImageOutputDimensions(t, output.Get("result").String(), 1024, 1024)
				require.True(t, output.Get("sub2api_image.postprocessed").Bool())
				require.Equal(t, "different", output.Get("sub2api_image.model_status").String())
			})
		}
	}
}

type imageOutputStagedConn struct{ *stagedPassthroughConn }

func (c *imageOutputStagedConn) WriteJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, payload)
}

func TestOpenAIImageOutputWebSocketClientTurns(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 10
			cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 10
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			upstream := &imageOutputStagedConn{newStagedPassthroughConn()}
			svc := newPassthroughLifecycleService(cfg, upstream.stagedPassthroughConn)
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(&stagedPassthroughDialer{conn: upstream})
			svc.openaiWSPool = pool
			defer pool.Close()
			account := passthroughLifecycleAccount()
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			results := make(chan *OpenAIForwardResult, 3)
			server, _ := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(*gin.Context) *OpenAIWSIngressHooks {
				return &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
					if err == nil {
						results <- result
					}
				}}
			})
			defer server.Close()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer client.CloseNow()
			encoded := encodeOpenAIImageTestPNG(t, 64, 64)
			for turn, size := range []int{32, 16, 0} {
				request := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","input":"turn %d","tools":[{"type":"image_generation","model":"gpt-image-2.6-flare","size":"%dx%d"}]}`, turn, size, size)
				if size == 0 {
					request = `{"type":"response.create","model":"gpt-5.4","input":"new turn","tools":[]}`
				}
				require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(request)))
				requirePassthroughUpstreamWrite(t, upstream.stagedPassthroughConn, 3*time.Second)
				upstream.Send(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_turn_%d","model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-2-codex"}],"output":[{"id":"ig_%d","type":"image_generation_call","result":%q}],"usage":{"input_tokens":5,"output_tokens":41,"total_tokens":46}}}`, turn, turn, encoded))
				var response []byte
				for {
					_, response, err = client.Read(ctx)
					require.NoError(t, err)
					if gjson.GetBytes(response, "type").String() == "response.completed" {
						break
					}
				}
				item := gjson.GetBytes(response, "response.output.0")
				if size == 0 {
					require.Equal(t, encoded, item.Get("result").String())
					require.False(t, item.Get("sub2api_image").Exists())
				} else {
					requireImageOutputDimensions(t, item.Get("result").String(), size, size)
					require.Equal(t, "different", item.Get("sub2api_image.model_status").String())
				}
				select {
				case result := <-results:
					require.Equal(t, 41, result.Usage.OutputTokens)
					// The passthrough relay exposes token accounting; the pool
					// additionally exposes image-size accounting. Neither input
					// may be replaced with the locally resized result.
					if mode == OpenAIWSIngressModeCtxPool {
						require.Equal(t, []string{"64x64"}, result.ImageOutputSizes)
					}
				case <-ctx.Done():
					t.Fatal("missing image accounting")
				}
			}
			_ = client.Close(coderws.StatusNormalClosure, "done")
		})
	}
}

func TestOpenAIImageOutputWebSocketHTTPBridge(t *testing.T) {
	encoded := encodeOpenAIImageTestPNG(t, 64, 64)
	event := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_bridge_image","model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-2-codex"}],"output":[{"id":"ig_bridge","type":"image_generation_call","result":%q}],"usage":{"input_tokens":5,"output_tokens":41,"total_tokens":46}}}`, encoded)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + event + "\n\n"))}}
	svc := newOpenAIImagesTestService(upstream)
	c, _ := newOpenAIImagesTestContext(t, nil)
	payload := []byte(`{"type":"response.create","model":"gpt-5.4","input":"draw","tools":[{"type":"image_generation","model":"gpt-image-2.6-flare","size":"32x32"}]}`)
	var output gjson.Result
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, directImagesTestAccount(), "test-token", payload, len(payload), "gpt-5.4", "gpt-image-2.6-flare", "1K", "32x32", "", 1, func(data []byte) error {
		if gjson.GetBytes(data, "type").String() == "response.completed" {
			output = gjson.GetBytes(data, "response.output.0")
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"64x64"}, result.ImageOutputSizes)
	requireImageOutputDimensions(t, output.Get("result").String(), 32, 32)
	require.Equal(t, "different", output.Get("sub2api_image.model_status").String())
}
