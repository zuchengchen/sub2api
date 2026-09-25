package middleware

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type bpsImageTestSettings struct {
	enabled      bool
	bodyLimitMiB int
	budgetMiB    int
	maxRequests  int
	err          error
}

func (s bpsImageTestSettings) GetExcelBPSImageRelaySettings(context.Context) (service.ExcelBPSImageRelaySettings, error) {
	return service.ExcelBPSImageRelaySettings{
		Enabled: s.enabled, BodyLimitMiB: s.bodyLimitMiB, BudgetMiB: s.budgetMiB, MaxRequests: s.maxRequests,
	}, s.err
}

type bpsImageCountingBody struct {
	reads  *atomic.Int32
	reader io.Reader
}

func (b *bpsImageCountingBody) Read(p []byte) (int, error) { b.reads.Add(1); return b.reader.Read(p) }
func (b *bpsImageCountingBody) Close() error               { return nil }

func bpsImageTestRouter(settings bpsImageTestSettings, next gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(string(ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformOpenAI}})
		c.Next()
	})
	r.Use(ExcelBPSImageAdmission(settings, 256<<20))
	for _, path := range []string{"/responses", "/v1/responses", "/backend-api/codex/responses", "/v1/chat/completions", "/chat/completions", "/v1/messages"} {
		r.POST(path, next)
	}
	r.POST("/v1/responses/*subpath", next)
	return r
}

func TestExcelBPSImageAdmission200ConcurrentRequests(t *testing.T) {
	for _, tt := range []struct {
		name     string
		length   int64
		encoding string
		allowed  int
	}{
		{"small", 1024, "", 32},
		{"large", 32 << 20, "", 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var reads atomic.Int32
			var active atomic.Int32
			var peak atomic.Int32
			entered := make(chan struct{}, 200)
			release := make(chan struct{})
			var once sync.Once
			var wg sync.WaitGroup
			t.Cleanup(func() { once.Do(func() { close(release) }); wg.Wait() })
			r := bpsImageTestRouter(bpsImageTestSettings{enabled: true}, func(c *gin.Context) {
				n := active.Add(1)
				for old := peak.Load(); n > old; old = peak.Load() {
					if peak.CompareAndSwap(old, n) {
						break
					}
				}
				_, err := io.Copy(io.Discard, c.Request.Body)
				if err != nil {
					t.Error(err)
				}
				entered <- struct{}{}
				<-release
				active.Add(-1)
				c.Status(http.StatusNoContent)
			})
			start := make(chan struct{})
			results := make(chan *httptest.ResponseRecorder, 200)
			paths := []string{"/responses", "/v1/responses/compact", "/backend-api/codex/responses", "/chat/completions", "/v1/messages"}
			for i := 0; i < 200; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					req := httptest.NewRequest(http.MethodPost, paths[i%len(paths)], nil)
					req.ContentLength = tt.length
					req.Header.Set("Content-Encoding", tt.encoding)
					req.Body = &bpsImageCountingBody{reads: &reads, reader: strings.NewReader("test")}
					w := httptest.NewRecorder()
					r.ServeHTTP(w, req)
					results <- w
				}(i)
			}
			close(start)
			deadline := time.After(10 * time.Second)
			for i := 0; i < 200-tt.allowed; i++ {
				select {
				case w := <-results:
					require.Equal(t, http.StatusServiceUnavailable, w.Code)
					require.Equal(t, "1", w.Header().Get("Retry-After"))
					require.Contains(t, w.Body.String(), "basispoints_image_request_busy")
				case <-deadline:
					t.Fatal("rejected requests did not finish without reading their body")
				}
			}
			for i := 0; i < tt.allowed; i++ {
				select {
				case <-entered:
				case <-deadline:
					t.Fatal("admitted requests did not enter")
				}
			}
			require.Equal(t, int32(tt.allowed), peak.Load())
			require.Equal(t, int32(tt.allowed*2), reads.Load(), "only admitted bodies may be read")
			once.Do(func() { close(release) })
			for i := 0; i < tt.allowed; i++ {
				select {
				case w := <-results:
					require.Equal(t, http.StatusNoContent, w.Code)
				case <-deadline:
					t.Fatal("admitted request stuck")
				}
			}
			wg.Wait()
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader("next"))
			r.ServeHTTP(w, req)
			require.Equal(t, http.StatusNoContent, w.Code, "completed requests must release their budget")
		})
	}
}

func TestExcelBPSImageAdmissionSmallBodyDoesNotHoldWorstCaseBudget(t *testing.T) {
	const body = `{"model":"gpt-6-astra","input":"hello"}`
	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	_, err := zipper.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, zipper.Close())

	for _, tt := range []struct {
		name     string
		wire     []byte
		length   int64
		encoding string
	}{
		{"chunked", []byte(body), -1, ""},
		{"gzip", compressed.Bytes(), int64(compressed.Len()), "gzip"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			type observed struct {
				body     string
				length   int64
				encoding string
			}
			entered := make(chan observed, 1)
			release := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			r := bpsImageTestRouter(bpsImageTestSettings{enabled: true}, func(c *gin.Context) {
				read, readErr := io.ReadAll(c.Request.Body)
				if readErr != nil {
					c.Status(http.StatusBadRequest)
					return
				}
				if c.GetHeader("Hold") == "true" {
					entered <- observed{string(read), c.Request.ContentLength, c.GetHeader("Content-Encoding")}
					<-release
				}
				c.Status(http.StatusNoContent)
			})
			first := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(tt.wire))
			first.ContentLength = tt.length
			first.Header.Set("Hold", "true")
			if tt.encoding != "" {
				first.Header.Set("Content-Encoding", tt.encoding)
			}
			firstResult := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				recorder := httptest.NewRecorder()
				r.ServeHTTP(recorder, first)
				firstResult <- recorder
			}()
			select {
			case got := <-entered:
				require.Equal(t, body, got.body)
				require.Equal(t, int64(len(body)), got.length)
				require.Empty(t, got.encoding)
			case <-time.After(5 * time.Second):
				t.Fatal("first request did not reach the handler")
			}
			second := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			secondResult := httptest.NewRecorder()
			r.ServeHTTP(secondResult, second)
			require.Equal(t, http.StatusNoContent, secondResult.Code, secondResult.Body.String())
			once.Do(func() { close(release) })
			select {
			case result := <-firstResult:
				require.Equal(t, http.StatusNoContent, result.Code)
			case <-time.After(5 * time.Second):
				t.Fatal("first request did not finish")
			}
		})
	}
}

func TestExcelBPSImageAdmissionConfiguredLimits(t *testing.T) {
	settings := bpsImageTestSettings{enabled: true, bodyLimitMiB: 1, budgetMiB: 512, maxRequests: 1}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	r := bpsImageTestRouter(settings, func(c *gin.Context) {
		if c.GetHeader("Hold") == "true" {
			entered <- struct{}{}
			<-release
		}
		c.Status(http.StatusNoContent)
	})
	first := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("small"))
	first.Header.Set("Hold", "true")
	firstResult := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, first)
		firstResult <- recorder
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not reach handler")
	}
	second := httptest.NewRecorder()
	r.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("small")))
	require.Equal(t, http.StatusServiceUnavailable, second.Code)
	require.Contains(t, second.Body.String(), "basispoints_image_request_busy")
	once.Do(func() { close(release) })
	select {
	case result := <-firstResult:
		require.Equal(t, http.StatusNoContent, result.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not finish")
	}
	tooLarge := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("small"))
	tooLarge.ContentLength = 2 << 20
	result := httptest.NewRecorder()
	r.ServeHTTP(result, tooLarge)
	require.Equal(t, http.StatusRequestEntityTooLarge, result.Code)
}

func TestExcelBPSImageAdmissionLimitsAndDisabled(t *testing.T) {
	for _, tt := range []struct {
		name     string
		settings bpsImageTestSettings
		length   int64
		body     string
		status   int
		wantRead bool
	}{
		{"oversized", bpsImageTestSettings{enabled: true}, 65 << 20, "body", 413, false},
		{"settings unavailable", bpsImageTestSettings{err: errors.New("private database error")}, 4, "body", 503, false},
		{"disabled", bpsImageTestSettings{}, 65 << 20, "body", 204, true},
		{"understated length", bpsImageTestSettings{enabled: true}, 1, "body", 413, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var reads atomic.Int32
			r := bpsImageTestRouter(tt.settings, func(c *gin.Context) {
				_, err := io.Copy(io.Discard, c.Request.Body)
				if err != nil {
					c.Status(413)
					return
				}
				c.Status(204)
			})
			req := httptest.NewRequest(http.MethodPost, "/responses", nil)
			req.ContentLength = tt.length
			req.Body = &bpsImageCountingBody{reads: &reads, reader: strings.NewReader(tt.body)}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, tt.status, w.Code)
			require.Equal(t, tt.wantRead, reads.Load() > 0)
			require.NotContains(t, w.Body.String(), "private database error")
		})
	}
}

func TestExcelBPSImageAdmissionReleaseIsIdempotent(t *testing.T) {
	budget := &bpsImageAdmissionBudget{}
	release, ok := budget.acquire(bpsImageBudgetBytes)
	require.True(t, ok)
	_, ok = budget.acquire(1)
	require.False(t, ok)
	release.release()
	release.release()
	require.Zero(t, budget.bytes)
	require.Zero(t, budget.requests)
}

func TestExcelBPSImageAdmissionReservationResize(t *testing.T) {
	budget := &bpsImageAdmissionBudget{}
	first, ok := budget.acquire(8 << 20)
	require.True(t, ok)
	second, ok := budget.acquire(8 << 20)
	require.True(t, ok)
	require.True(t, first.resize(504<<20))
	require.False(t, second.resize(16<<20))
	require.Equal(t, int64(512<<20), budget.bytes)
	require.True(t, first.resize(8<<20))
	require.True(t, second.resize(16<<20))
	second.release()
	first.release()
	require.Zero(t, budget.bytes)
	require.Zero(t, budget.requests)
}

func TestExcelBPSImageAdmissionUnknownBodyStopsBeforeBudgetOverflow(t *testing.T) {
	budget := &bpsImageAdmissionBudget{}
	first, ok := budget.acquire(504 << 20)
	require.True(t, ok)
	defer first.release()
	second, ok := budget.acquire(8 << 20)
	require.True(t, ok)
	defer second.release()
	var reads atomic.Int32
	body := &bpsImageBudgetedBody{
		ReadCloser:  &bpsImageCountingBody{reads: &reads, reader: strings.NewReader("content")},
		reservation: second,
		maxBody:     bpsImageMaxBodyBytes,
	}
	_, err := body.Read(make([]byte, 2<<20))
	require.ErrorIs(t, err, errBPSImageRequestBusy)
	require.Zero(t, reads.Load(), "body must not be read after the reservation fails")
}

func TestExcelBPSImageAdmissionReleasesAfterCancellation(t *testing.T) {
	entered := make(chan struct{})
	done := make(chan struct{})
	r := bpsImageTestRouter(bpsImageTestSettings{enabled: true}, func(c *gin.Context) {
		if c.GetHeader("Hold") == "true" {
			close(entered)
			<-c.Request.Context().Done()
		}
		c.Status(http.StatusNoContent)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/responses", nil).WithContext(ctx)
	req.ContentLength = -1
	req.Header.Set("Hold", "true")
	go func() {
		r.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request did not finish")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/responses", nil))
	require.Equal(t, http.StatusNoContent, w.Code)
}
