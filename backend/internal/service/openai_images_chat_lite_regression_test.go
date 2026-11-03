//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIUsageRegressionMultipartImageWithDefaultContentType(t *testing.T) {
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("model", "gpt-image-2"))
	require.NoError(t, writer.WriteField("prompt", "synthetic edit"))
	for _, field := range []string{"image", "mask"} {
		part, err := writer.CreateFormFile(field, "synthetic.png")
		require.NoError(t, err)
		_, err = part.Write(pixels.Bytes())
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	c, _ := newOpenAIImagesTestContext(t, body.Bytes())
	c.Request.URL.Path = "/v1/images/edits"
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	svc := newOpenAIImagesTestService(nil)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body.Bytes())
	require.NoError(t, err)
	require.Equal(t, "application/octet-stream", parsed.Uploads[0].ContentType)
	got, _, err := buildOpenAIImagesOAuthPayload(parsed, parsed.Model)
	require.NoError(t, err)
	for _, path := range []string{"images.0.image_url", "mask.image_url"} {
		mimePrefix := strings.SplitN(gjson.GetBytes(got, path).String(), ";", 2)[0]
		require.Equal(t, "data:image/png", mimePrefix, "gateway must preserve detected image format when converting generic multipart data: %s", path)
	}
}

func TestOpenAIUsageRegressionChatLiteHeaderRequiresMatchingBody(t *testing.T) {
	for _, lite := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "lite_header"}[lite], func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"synthetic hello"}],"reasoning_effort":"high","stream":false}`)
			upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusOK, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_synthetic\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")}
			upstream.resp.Header.Set("Content-Type", "text/event-stream")
			svc := newOpenAIRejectedFieldTestService(upstream)
			c := newOpenAIRejectedFieldTestContext(body)
			c.Request.URL.Path = "/v1/chat/completions"
			if lite {
				c.Request.Header.Set(responsesLiteHeader, "true")
			}
			result, err := svc.ForwardAsChatCompletions(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			if isOpenAIResponsesLiteHeader(upstream.lastReq.Header.Get(responsesLiteHeader)) {
				require.Equal(t, "all_turns", gjson.GetBytes(upstream.lastBody, "reasoning.context").String(), "outbound Lite header and body must agree")
			}
		})
	}
}

func TestOpenAIUsageRegressionImageUploadMIMEBoundaries(t *testing.T) {
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	for _, tc := range []struct {
		name, declared, want string
		data                 []byte
		moderation           bool
	}{
		{"missing", "", "image/png", pixels.Bytes(), true},
		{"generic", "application/octet-stream", "image/png", pixels.Bytes(), true},
		{"generic_params", " APPLICATION/OCTET-STREAM; charset=binary ", "image/png", pixels.Bytes(), true},
		{"binary_generic", "binary/octet-stream", "image/png", pixels.Bytes(), true},
		{"explicit_image", "image/png", "image/png", pixels.Bytes(), true},
		{"explicit_other", "application/pdf", "application/pdf", pixels.Bytes(), false},
		{"non_image_generic", "application/octet-stream", "application/octet-stream", []byte("not an image despite png filename"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upload := OpenAIImagesUpload{FileName: "synthetic.png", ContentType: tc.declared, Data: tc.data}
			got, err := openAIImageUploadToDataURL(upload)
			require.NoError(t, err)
			parts := strings.SplitN(got, ";base64,", 2)
			require.Len(t, parts, 2)
			require.Equal(t, "data:"+tc.want, parts[0])
			decoded, err := base64.StdEncoding.DecodeString(parts[1])
			require.NoError(t, err)
			require.Equal(t, tc.data, decoded, "MIME repair must not alter image bytes")
			moderation := upload.ModerationDataURL()
			if tc.moderation {
				require.Equal(t, got, moderation)
			} else {
				require.Empty(t, moderation)
			}
		})
	}
	upload := OpenAIImagesUpload{FileName: "synthetic.png", ContentType: "application/octet-stream", Data: pixels.Bytes()}
	req := &OpenAIImagesRequest{Endpoint: openAIImagesEditsEndpoint, Model: "gpt-image-1", Prompt: "synthetic edit", Uploads: []OpenAIImagesUpload{upload}, MaskUpload: &upload}
	got, err := buildOpenAIImagesResponsesRequest(req, req.Model)
	require.NoError(t, err)
	for _, path := range []string{"input.0.content.1.image_url", "tools.0.input_image_mask.image_url"} {
		require.True(t, strings.HasPrefix(gjson.GetBytes(got, path).String(), "data:image/png;base64,"), path)
	}
}

func TestOpenAIUsageRegressionChatLitePreservesNamespaceAndHistory(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"synthetic history"}],"reasoning":{"effort":"high","context":"current_turn"},"tools":[{"type":"namespace","name":"audit_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}],"tool_choice":{"type":"namespace","name":"audit_tools"},"parallel_tool_calls":true,"stream":false}`)
	original := append([]byte(nil), body...)
	upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusOK, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_synthetic\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")}
	upstream.resp.Header.Set("Content-Type", "text/event-stream")
	svc := newOpenAIRejectedFieldTestService(upstream)
	c := newOpenAIRejectedFieldTestContext(body)
	c.Request.URL.Path = "/v1/chat/completions"
	c.Request.Header.Set(responsesLiteHeader, "true")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
	require.NoError(t, err)
	require.Equal(t, original, body, "compatibility must not mutate the ingress body reused by failover")
	require.Equal(t, "true", c.GetHeader(responsesLiteHeader))
	require.Equal(t, "true", upstream.lastReq.Header.Get(responsesLiteHeader))
	require.Equal(t, "all_turns", gjson.GetBytes(upstream.lastBody, "reasoning.context").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Bool())
	require.Equal(t, "audit_tools", gjson.GetBytes(upstream.lastBody, `input.#(type=="additional_tools").tools.0.name`).String())
	require.Equal(t, "lookup", gjson.GetBytes(upstream.lastBody, `input.#(type=="additional_tools").tools.0.tools.0.name`).String())
	require.Equal(t, "namespace", gjson.GetBytes(upstream.lastBody, "tool_choice.type").String())
	require.Equal(t, "audit_tools", gjson.GetBytes(upstream.lastBody, "tool_choice.name").String())
	require.Contains(t, string(upstream.lastBody), "synthetic history")
}

func TestOpenAIUsageRegressionChatLiteRejectsUnsupportedToolWithoutDroppingIntent(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"synthetic history","tools":[{"type":"image_generation"}],"stream":false}`)
	original := append([]byte(nil), body...)
	upstream := &httpUpstreamRecorder{}
	svc := newOpenAIRejectedFieldTestService(upstream)
	c := newOpenAIRejectedFieldTestContext(body)
	c.Request.URL.Path = "/v1/chat/completions"
	c.Request.Header.Set(responsesLiteHeader, "true")
	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
	require.ErrorContains(t, err, "does not support top-level tool type")
	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
	require.Empty(t, upstream.requests)
	require.Equal(t, original, body)
}
