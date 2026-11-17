//go:build unit

package handler

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

type oauthReliabilityLargeHandlerUpstream struct {
	service.HTTPUpstream
	t     *testing.T
	wires [][]byte
}

func (u *oauthReliabilityLargeHandlerUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	require.NoError(u.t, err)
	require.NoError(u.t, req.Body.Close())
	require.EqualValues(u.t, len(body), req.ContentLength)
	require.Empty(u.t, req.Header.Get("Content-Encoding"), "the default outbound policy preserves decoded JSON")
	u.wires = append(u.wires, body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: " + `{"type":"response.completed","response":{"id":"resp_large_contract","model":"gpt-5.1","status":"completed","output":[{"type":"message","id":"msg_large_contract","role":"assistant","content":[{"type":"output_text","text":"Complete synthetic request received."}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n")),
		Request:    req,
	}, nil
}

func oauthReliabilityLargeHandlerWire(t *testing.T, route, encoding string, size int) ([]byte, string) {
	t.Helper()
	prefix, suffix := `{"model":"gpt-5.1","stream":false,"input":"`, `"}`
	if route == "chat" {
		prefix, suffix = `{"model":"gpt-5.1","stream":false,"messages":[{"role":"user","content":"`, `"}]}`
	} else if route == "messages" {
		prefix, suffix = `{"model":"gpt-5.1","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"`, `"}]}`
	}
	content := "synthetic-complete-start-" + strings.Repeat("x", size-len(prefix)-len(suffix)-len("synthetic-complete-start--complete-end")) + "-complete-end"
	plain := []byte(prefix + content + suffix)
	require.Len(t, plain, size)
	if encoding == "identity" {
		return plain, content
	}
	var wire bytes.Buffer
	var writer io.WriteCloser
	var err error
	switch encoding {
	case "gzip":
		writer, err = gzip.NewWriterLevel(&wire, gzip.BestSpeed)
	case "zstd":
		writer, err = zstd.NewWriter(&wire, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
	case "deflate":
		writer, err = zlib.NewWriterLevel(&wire, zlib.BestSpeed)
	default:
		t.Fatalf("unknown fixture encoding %q", encoding)
	}
	require.NoError(t, err)
	_, err = writer.Write(plain)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return wire.Bytes(), content
}

func oauthReliabilityLargeHandlerForward(t *testing.T, route, encoding string, wire []byte, limit int64) (*httptest.ResponseRecorder, *oauthReliabilityLargeHandlerUpstream) {
	t.Helper()
	upstream := &oauthReliabilityLargeHandlerUpstream{t: t}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream)
	h.cfg.Gateway.MaxBodySize = limit
	c, recorder := newOpenAIResponsesFailoverTestContext(t, context.Background())
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	require.True(t, ok)
	apiKey.Group.AllowMessagesDispatch = true
	path := "/v1/responses"
	if route == "chat" {
		path = "/v1/chat/completions"
	} else if route == "messages" {
		path = "/v1/messages"
	}
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(wire))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("Content-Encoding", encoding)
	// The real route middleware bounds encoded bytes; the shared reader must
	// also enforce the decoded/normalized request budget before dispatch.
	c.Request.Body = http.MaxBytesReader(recorder, c.Request.Body, limit)
	switch route {
	case "chat":
		h.ChatCompletions(c)
	case "messages":
		h.Messages(c)
	default:
		h.Responses(c)
	}
	return recorder, upstream
}

func TestOAuthReliabilityLargeRequestHandlerConfiguredBoundaries(t *testing.T) {
	const limit = 4096
	for _, route := range []string{"responses", "chat", "messages"} {
		for _, encoding := range []string{"identity", "gzip", "zstd", "deflate"} {
			for _, size := range []int{limit - 1, limit, limit + 1} {
				t.Run(fmt.Sprintf("%s/%s/%d", route, encoding, size), func(t *testing.T) {
					wire, content := oauthReliabilityLargeHandlerWire(t, route, encoding, size)
					recorder, upstream := oauthReliabilityLargeHandlerForward(t, route, encoding, wire, limit)
					if size > limit {
						require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code, recorder.Body.String())
						require.Contains(t, recorder.Body.String(), "4096B")
						require.Empty(t, upstream.wires, "local body rejection must precede any upstream execution")
						return
					}
					require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
					require.Len(t, upstream.wires, 1)
					require.True(t, json.Valid(upstream.wires[0]))
					require.Contains(t, string(upstream.wires[0]), content, "the complete input, including its tail, must reach upstream once")
					require.Contains(t, recorder.Body.String(), "Complete synthetic request received.")
				})
			}
		}
	}
}

func TestOAuthReliabilityLargeRequestHandlerDecodedOverflowIs413(t *testing.T) {
	for _, tc := range []struct{ route, encoding string }{
		{"responses", "gzip"}, {"chat", "zstd"}, {"messages", "deflate"},
	} {
		t.Run(tc.route+"/"+tc.encoding, func(t *testing.T) {
			wire, _ := oauthReliabilityLargeHandlerWire(t, tc.route, tc.encoding, 64<<20+1)
			recorder, upstream := oauthReliabilityLargeHandlerForward(t, tc.route, tc.encoding, wire, 256<<20)
			require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code, "a valid oversized request is a local body-limit failure, not malformed JSON: %s", recorder.Body.String())
			require.Contains(t, recorder.Body.String(), "64MB")
			require.NotContains(t, recorder.Body.String(), "context_length_exceeded")
			require.Empty(t, upstream.wires)
		})
	}
}

func TestOAuthReliabilityLargeRequestHandlerCorruptTailIs400(t *testing.T) {
	for _, tc := range []struct{ route, encoding string }{
		{"responses", "gzip"}, {"chat", "zstd"}, {"messages", "deflate"},
	} {
		t.Run(tc.route+"/"+tc.encoding, func(t *testing.T) {
			wire, _ := oauthReliabilityLargeHandlerWire(t, tc.route, tc.encoding, 4096)
			wire[len(wire)-4] ^= 0x80
			recorder, upstream := oauthReliabilityLargeHandlerForward(t, tc.route, tc.encoding, wire, 8192)
			require.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
			require.NotContains(t, recorder.Body.String(), "too large")
			require.Empty(t, upstream.wires)
		})
	}
}
