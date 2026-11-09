//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpsErrorLoggerLocalCancellationKeepsUpstreamAttempt(t *testing.T) {
	for _, scenario := range []struct {
		name, message string
		deadline      bool
	}{
		{"cancellation", "was canceled", false}, {"deadline", "deadline expired", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 2)
			gin.SetMode(gin.TestMode)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{{
					AccountID: 7, UpstreamStatusCode: 503, Kind: "failover", Message: "upstream overloaded",
				}})
				failoverClientGone(c)
			})
			var ctx context.Context
			var cancel context.CancelFunc
			if scenario.deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			}
			defer cancel()
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx))
			require.Equal(t, 499, recorder.Code)
			require.Empty(t, recorder.Body.String())
			require.Equal(t, int64(1), OpsErrorLogQueueLength())
			job := <-opsErrorLogQueue
			require.Equal(t, 499, job.entry.StatusCode)
			require.Equal(t, "upstream", job.entry.ErrorPhase)
			require.Contains(t, job.entry.ErrorMessage, scenario.message)
			require.Empty(t, job.entry.ErrorBody, "no response body was sent; do not fabricate one")
			require.NotNil(t, job.entry.UpstreamStatusCode)
			require.Equal(t, 503, *job.entry.UpstreamStatusCode)
			require.NotNil(t, job.entry.UpstreamErrorsJSON)
			attempts, err := service.ParseOpsUpstreamErrors(*job.entry.UpstreamErrorsJSON)
			require.NoError(t, err)
			require.Len(t, attempts, 1)
			require.Equal(t, 503, attempts[0].UpstreamStatusCode)
			require.Equal(t, "failover", attempts[0].Kind)
		})
	}
}

func TestOpsErrorLoggerUpstream499KeepsProviderMessage(t *testing.T) {
	setupOpsErrorLogTestQueue(t, 2)
	gin.SetMode(gin.TestMode)
	ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.Use(OpsErrorLoggerMiddleware(ops))
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set(service.OpsUpstreamErrorsKey, []*service.OpsUpstreamErrorEvent{{
			AccountID: 7, UpstreamStatusCode: 499, Kind: "http_error", Message: "provider canceled operation",
		}})
		c.JSON(499, gin.H{"error": gin.H{"type": "upstream_error", "message": "provider canceled operation"}})
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	require.Equal(t, int64(1), OpsErrorLogQueueLength())
	job := <-opsErrorLogQueue
	require.Equal(t, "provider canceled operation", job.entry.ErrorMessage)
	require.Equal(t, 499, job.entry.StatusCode)
	require.Equal(t, 499, *job.entry.UpstreamStatusCode)
}
