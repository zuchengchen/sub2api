package repository

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestAccountTrafficProtectionRemovedHTTPTransportLifecycle(t *testing.T) {
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	upstream := NewHTTPUpstream(nil).(*httpUpstreamService)
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(`{"input":"test"}`))
			if err != nil {
				results <- err
				return
			}
			resp, err := upstream.Do(req, "", 7, 50)
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				closeErr := resp.Body.Close()
				if err == nil {
					err = closeErr
				}
			}
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.Equal(t, int32(12), received.Load())
	requireNoUpstreamInFlight(t, upstream)
}

func TestAccountTrafficProtectionRemovedHistoricalResultsRemainReadable(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	now := time.Now().UTC()
	columns := []string{"id", "account_id", "test_type", "status", "score", "result", "result_image", "input", "raw_response", "raw_truncated", "error_message", "duration_ms", "model", "anti_degradation", "config_snapshot", "evaluation", "started_at", "finished_at", "created_at", "lease_token", "queue_reason", "available_at"}
	rows := sqlmock.NewRows(columns).AddRow(1, 7, "pelican", "success", 1.0, "historical result", "", "old prompt", "raw", false, "", 120, "gpt-6-astra", true, []byte(`{"execution":{"strategy":"legacy","identity_mode":"full","effective_tls":"nodejs24","concurrency":50,"integrity_mode":"observe"}}`), []byte(`{}`), now, now, now, "", "", now)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT " + intelligentRecordColumns + " FROM account_tests t WHERE t.id=$1")).WithArgs(int64(1)).WillReturnRows(rows)
	record, err := NewIntelligentTestRepository(db).Get(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "historical result", record.Result)
	require.True(t, record.AntiDegradation)
	require.Equal(t, "legacy", record.ConfigSnapshot.Execution.Strategy)
	require.Equal(t, "full", record.ConfigSnapshot.Execution.IdentityMode)
	require.Equal(t, 50, *record.ConfigSnapshot.Execution.Concurrency)
	require.Equal(t, "nodejs24", record.ConfigSnapshot.Execution.EffectiveTLS)
	require.NoError(t, mock.ExpectationsWereMet())
}
