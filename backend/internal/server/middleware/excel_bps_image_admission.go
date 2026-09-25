package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	bpsImageMaxBodyBytes   = 128 << 20
	bpsImageBudgetBytes    = 512 << 20
	bpsImageBodyMultiplier = 8
	bpsImageMinBodyBytes   = 1 << 20
	bpsImageMaxRequests    = 32
)

type excelBPSImageSettingsReader interface {
	GetExcelBPSImageRelaySettings(context.Context) (service.ExcelBPSImageRelaySettings, error)
}

// This budget accounts for request bodies and their processing copies, not RSS.
// Reserve before any body-reading middleware and hold until the request ends,
// including upstream streaming and scheduler waits. Never queue large bodies.
type bpsImageAdmissionBudget struct {
	mu          sync.Mutex
	bytes       int64
	requests    int
	limitBytes  int64
	maxRequests int
}

type bpsImageReservation struct {
	budget *bpsImageAdmissionBudget
	weight int64
	once   sync.Once
}

func (b *bpsImageAdmissionBudget) acquire(weight int64) (*bpsImageReservation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	limitBytes, maxRequests := b.limits()
	if b.requests >= maxRequests || weight > limitBytes-b.bytes {
		return nil, false
	}
	b.bytes += weight
	b.requests++
	return &bpsImageReservation{budget: b, weight: weight}, true
}

func (b *bpsImageAdmissionBudget) limits() (int64, int) {
	limitBytes, maxRequests := b.limitBytes, b.maxRequests
	if limitBytes == 0 {
		limitBytes = bpsImageBudgetBytes
	}
	if maxRequests == 0 {
		maxRequests = bpsImageMaxRequests
	}
	return limitBytes, maxRequests
}

func (b *bpsImageAdmissionBudget) configure(limitBytes int64, maxRequests int) {
	b.mu.Lock()
	b.limitBytes, b.maxRequests = limitBytes, maxRequests
	b.mu.Unlock()
}

func (r *bpsImageReservation) resize(weight int64) bool {
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	limitBytes, _ := b.limits()
	if weight > r.weight && weight-r.weight > limitBytes-b.bytes {
		return false
	}
	b.bytes += weight - r.weight
	r.weight = weight
	return true
}

func (r *bpsImageReservation) release() {
	r.once.Do(func() {
		b := r.budget
		b.mu.Lock()
		defer b.mu.Unlock()
		b.bytes -= r.weight
		b.requests--
	})
}

var errBPSImageRequestBusy = errors.New("image relay request capacity is busy")

type bpsImageBudgetedBody struct {
	io.ReadCloser
	reservation *bpsImageReservation
	read        int64
	maxBody     int64
}

func (b *bpsImageBudgetedBody) Read(p []byte) (int, error) {
	anticipated := b.read + int64(len(p))
	if anticipated > b.maxBody {
		anticipated = b.maxBody
	}
	if anticipated < bpsImageMinBodyBytes {
		anticipated = bpsImageMinBodyBytes
	}
	if !b.reservation.resize(anticipated * bpsImageBodyMultiplier) {
		return 0, errBPSImageRequestBusy
	}
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	return n, err
}

// ExcelBPSImageAdmission must be shared across the gateway route aliases.
// Account selection occurs after reading JSON, so enabling image relay applies
// this guard to OpenAI/Composite Responses, Chat and Messages HTTP requests,
// including text-only requests. Disabled relay leaves existing limits intact.
func ExcelBPSImageAdmission(settings excelBPSImageSettingsReader, configuredMax int64) gin.HandlerFunc {
	budget := &bpsImageAdmissionBudget{}
	return func(c *gin.Context) {
		if settings == nil || !bpsImageAdmissionRoute(c) {
			c.Next()
			return
		}
		key, ok := GetAPIKeyFromContext(c)
		if !ok || key == nil || (key.Group != nil && key.Group.Platform != service.PlatformOpenAI && key.Group.Platform != service.PlatformComposite) {
			c.Next()
			return
		}
		relay, err := settings.GetExcelBPSImageRelaySettings(c.Request.Context())
		if err != nil {
			bpsImageAdmissionError(c, http.StatusServiceUnavailable, "basispoints_image_settings_unavailable", "Image relay settings are unavailable")
			return
		}
		if !relay.Enabled {
			c.Next()
			return
		}
		bodyLimitMiB, budgetMiB, maxRequests := relay.BodyLimitMiB, relay.BudgetMiB, relay.MaxRequests
		if bodyLimitMiB == 0 {
			bodyLimitMiB = service.DefaultExcelBPSImageBodyLimitMiB
		}
		if budgetMiB == 0 {
			budgetMiB = service.DefaultExcelBPSImageBudgetMiB
		}
		if maxRequests == 0 {
			maxRequests = service.DefaultExcelBPSImageMaxRequests
		}
		budget.configure(int64(budgetMiB)<<20, maxRequests)
		maxBody := int64(bodyLimitMiB) << 20
		if maxBody > bpsImageMaxBodyBytes {
			maxBody = bpsImageMaxBodyBytes
		}
		if configuredMax > 0 && configuredMax < maxBody {
			maxBody = configuredMax
		}
		length := c.Request.ContentLength
		if length > maxBody {
			bpsImageAdmissionError(c, http.StatusRequestEntityTooLarge, "basispoints_image_body_too_large", "Request body exceeds the image relay ingress limit")
			return
		}
		readLimit := maxBody
		if length > 0 {
			readLimit = length
		}
		encoding := strings.TrimSpace(c.GetHeader("Content-Encoding"))
		compressed := encoding != "" && !strings.EqualFold(encoding, "identity")
		// Compressed input needs the full budget while decoding, but only its
		// actual decoded size while the downstream response is in flight.
		accounted := readLimit
		if compressed {
			accounted = maxBody
		} else if length <= 0 {
			accounted = bpsImageMinBodyBytes
		}
		if accounted < bpsImageMinBodyBytes {
			accounted = bpsImageMinBodyBytes
		}
		reservation, acquired := budget.acquire(accounted * bpsImageBodyMultiplier)
		if !acquired {
			bpsImageAdmissionError(c, http.StatusServiceUnavailable, "basispoints_image_request_busy", "Image relay request capacity is busy; retry later")
			return
		}
		defer reservation.release()
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, readLimit)
		if length <= 0 || compressed {
			if !compressed {
				c.Request.Body = &bpsImageBudgetedBody{ReadCloser: c.Request.Body, reservation: reservation, maxBody: maxBody}
			}
			body, err := httputil.ReadRequestBodyWithPrealloc(c.Request)
			_ = c.Request.Body.Close()
			if err != nil {
				switch {
				case errors.Is(err, errBPSImageRequestBusy):
					bpsImageAdmissionError(c, http.StatusServiceUnavailable, "basispoints_image_request_busy", "Image relay request capacity is busy; retry later")
				default:
					var maxErr *http.MaxBytesError
					if errors.As(err, &maxErr) {
						bpsImageAdmissionError(c, http.StatusRequestEntityTooLarge, "basispoints_image_body_too_large", "Request body exceeds the image relay ingress limit")
					} else {
						c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "Failed to read request body"}})
					}
				}
				return
			}
			actual := int64(len(body))
			if actual > maxBody {
				bpsImageAdmissionError(c, http.StatusRequestEntityTooLarge, "basispoints_image_body_too_large", "Request body exceeds the image relay ingress limit")
				return
			}
			if actual < bpsImageMinBodyBytes {
				actual = bpsImageMinBodyBytes
			}
			reservation.resize(actual * bpsImageBodyMultiplier)
			c.Request.Body = httputil.NewPrereadBody(body)
			c.Request.ContentLength = int64(len(body))
		}
		c.Next()
	}
}

func bpsImageAdmissionRoute(c *gin.Context) bool {
	if c.Request.Method != http.MethodPost {
		return false
	}
	switch c.FullPath() {
	case "/responses", "/responses/*subpath", "/v1/responses", "/v1/responses/*subpath",
		"/backend-api/codex/responses", "/backend-api/codex/responses/*subpath",
		"/chat/completions", "/v1/chat/completions", "/v1/messages":
		return true
	}
	return false
}

func bpsImageAdmissionError(c *gin.Context, status int, code, message string) {
	errorType := "server_error"
	if status == http.StatusRequestEntityTooLarge {
		errorType = "invalid_request_error"
	}
	if status == http.StatusServiceUnavailable {
		c.Header("Retry-After", "1")
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"type": errorType, "code": code, "message": message}})
}
