package routes

import (
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	grokVidgenUpstream = "https://vidgen.x.ai/"
	grokImgenUpstream  = "https://imgen.x.ai/"
	grokVidgenHost     = "vidgen.x.ai"
	grokImgenHost      = "imgen.x.ai"
)

var grokMediaCDNClient = &http.Client{
	Timeout: 180 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   15 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
	},
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func registerGrokMediaCDNRoutes(r *gin.Engine) {
	// Unauthenticated: Grok CLI GETs video.url with no Authorization.
	// Must be registered before the SPA fallback and outside API-key groups.
	r.GET("/v1/media/vidgen/*object", handleGrokVidgenProxy)
	r.HEAD("/v1/media/vidgen/*object", handleGrokVidgenProxy)
	r.GET("/v1/media/imgen/*object", handleGrokImgenProxy)
	r.HEAD("/v1/media/imgen/*object", handleGrokImgenProxy)
}

func handleGrokVidgenProxy(c *gin.Context) {
	object := strings.TrimPrefix(c.Param("object"), "/")
	if !service.IsAllowedGrokVidgenObject(object) {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusNotFound, "not found")
		return
	}
	proxyOfficialMedia(c, grokVidgenUpstream+object, grokVidgenHost)
}

func handleGrokImgenProxy(c *gin.Context) {
	object := strings.TrimPrefix(c.Param("object"), "/")
	if !service.IsAllowedGrokImgenObject(object) {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusNotFound, "not found")
		return
	}
	proxyOfficialMedia(c, grokImgenUpstream+object, grokImgenHost)
}

func proxyOfficialMedia(c *gin.Context, upstreamURL, upstreamHost string) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, nil)
	if err != nil {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusBadGateway, "bad gateway")
		return
	}
	// Signed CDN URLs carry their authorization in the query string. Preserve
	// the exact encoded query while keeping the upstream host fixed above.
	req.URL.RawQuery = c.Request.URL.RawQuery
	req.Host = upstreamHost
	req.Header.Set("Host", upstreamHost)
	req.Header.Set("Accept", "*/*")
	if rangeHeader := strings.TrimSpace(c.GetHeader("Range")); rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	if inm := strings.TrimSpace(c.GetHeader("If-None-Match")); inm != "" {
		req.Header.Set("If-None-Match", inm)
	}
	if ims := strings.TrimSpace(c.GetHeader("If-Modified-Since")); ims != "" {
		req.Header.Set("If-Modified-Since", ims)
	}

	resp, err := grokMediaCDNClient.Do(req)
	if err != nil {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusBadGateway, "bad gateway")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		c.Header("Content-Type", "text/plain; charset=utf-8")
		c.String(http.StatusBadGateway, "bad gateway")
		return
	}

	for _, name := range []string{
		"Content-Type",
		"Content-Length",
		"Content-Range",
		"Accept-Ranges",
		"ETag",
		"Last-Modified",
		"Cache-Control",
	} {
		if value := strings.TrimSpace(resp.Header.Get(name)); value != "" {
			c.Header(name, value)
		}
	}
	if strings.TrimSpace(c.Writer.Header().Get("Content-Type")) == "" && resp.StatusCode < 400 {
		c.Header("Content-Type", "application/octet-stream")
	}
	c.Status(resp.StatusCode)
	if c.Request.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(c.Writer, resp.Body)
}
