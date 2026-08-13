package routes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type grokMediaCDNRoundTripper func(*http.Request) (*http.Response, error)

func (fn grokMediaCDNRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestGrokMediaCDNProxyPreservesSignedRequestAndResponseSemantics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var mu sync.Mutex
	var captured []*http.Request
	originalClient := grokMediaCDNClient
	grokMediaCDNClient = &http.Client{Transport: grokMediaCDNRoundTripper(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		captured = append(captured, req.Clone(req.Context()))
		mu.Unlock()
		responseHeader := make(http.Header)
		responseHeader.Set("Content-Type", "video/mp4")
		responseHeader.Set("Content-Range", "bytes 0-3/8")
		responseHeader.Set("Accept-Ranges", "bytes")
		responseHeader.Set("ETag", `"media-etag"`)
		responseHeader.Set("Content-Length", "4")
		return &http.Response{
			StatusCode: http.StatusPartialContent,
			Header:     responseHeader,
			Body:       io.NopCloser(strings.NewReader("data")),
		}, nil
	}), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() { grokMediaCDNClient = originalClient })

	router := gin.New()
	RegisterCommonRoutes(router)
	req := httptest.NewRequest(http.MethodGet, "/v1/media/vidgen/xai-vidgen-bucket/xai-video-1.mp4?Policy=a%2Bb&Signature=x%2Fy", nil)
	req.Header.Set("Range", "bytes=0-3")
	req.Header.Set("If-None-Match", `"client-etag"`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusPartialContent, recorder.Code)
	require.Equal(t, "data", recorder.Body.String())
	require.Equal(t, "bytes 0-3/8", recorder.Header().Get("Content-Range"))
	require.Equal(t, `"media-etag"`, recorder.Header().Get("ETag"))
	mu.Lock()
	require.Len(t, captured, 1)
	upstream := captured[0]
	mu.Unlock()
	require.Equal(t, "vidgen.x.ai", upstream.URL.Host)
	require.Equal(t, "/xai-vidgen-bucket/xai-video-1.mp4", upstream.URL.EscapedPath())
	require.Equal(t, "Policy=a%2Bb&Signature=x%2Fy", upstream.URL.RawQuery)
	require.Equal(t, "bytes=0-3", upstream.Header.Get("Range"))
	require.Equal(t, `"client-etag"`, upstream.Header.Get("If-None-Match"))

	head := httptest.NewRequest(http.MethodHead, "/v1/media/imgen/previews/frame-1.jpg?token=fake", nil)
	headRecorder := httptest.NewRecorder()
	router.ServeHTTP(headRecorder, head)
	require.Equal(t, http.StatusPartialContent, headRecorder.Code)
	require.Empty(t, headRecorder.Body.String())
}

func TestGrokMediaCDNProxyRejectsInvalidObjectsAndRedirects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	calls := 0
	originalClient := grokMediaCDNClient
	grokMediaCDNClient = &http.Client{Transport: grokMediaCDNRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": {"https://attacker.invalid/file"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
		}, nil
	}), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(func() { grokMediaCDNClient = originalClient })

	router := gin.New()
	RegisterCommonRoutes(router)
	for _, target := range []string{
		"/v1/media/vidgen/signed-token/video.mp4",
		"/v1/media/vidgen/xai-vidgen-bucket/dir/video.mp4",
		"/v1/media/vidgen/xai-vidgen-bucket/video.txt",
		"/v1/media/imgen/../private",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		require.Equal(t, http.StatusNotFound, recorder.Code, "target=%s", target)
	}
	require.Zero(t, calls)

	redirectRecorder := httptest.NewRecorder()
	router.ServeHTTP(redirectRecorder, httptest.NewRequest(http.MethodGet, "/v1/media/imgen/previews/frame.jpg", nil))
	require.Equal(t, http.StatusBadGateway, redirectRecorder.Code)
	require.Empty(t, redirectRecorder.Header().Get("Location"))
	require.Equal(t, 1, calls)
}
