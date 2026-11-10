//go:build integration && embed

package integration

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/tidwall/gjson"
)

// This test exercises actual routes, auth, Redis scheduling/concurrency, OAuth
// forwarding and PostgreSQL billing. Only the remote upstream is synthetic.
// It intentionally fails, rather than skips, when Docker is unavailable.
func TestCodexOAuthGatewayIntegration(t *testing.T) {
	require.NoError(t, timezone.Init("UTC"))
	gin.SetMode(gin.TestMode)
	store := newCodexIntegrationStore(t)
	upstream := newCodexIntegrationUpstream(t)
	for _, mode := range []string{"http", "passthrough", "ctx_pool"} {
		t.Run(mode, func(t *testing.T) {
			f := newCodexIntegrationGateway(t, store, upstream, mode)
			t.Run("auth_rejection_has_no_egress_or_charge", f.testRejection)
			if mode == "http" {
				t.Run("sse_complete_and_duplicate_terminal", f.testHTTPComplete)
				t.Run("same_account_cross_user_identity", f.testHTTPIsolation)
				t.Run("cancel_preserves_observed_usage", f.testHTTPCancel)
				t.Run("soft_429_retries_same_account_once_before_success", f.testHTTPSoft429)
				t.Run("scheduler_failover", f.testHTTPFailover)
			} else {
				t.Run("ws_complete", f.testWSComplete)
				t.Run("same_account_cross_user_identity", f.testWSIsolation)
				t.Run("response_replay_deduplicates_per_key", f.testWSReplay)
			}
			t.Run("ledger_balances_and_slots", f.testLedgerAndSlots)
		})
	}
	require.Empty(t, upstream.errorsSnapshot(), "local upstream/proxy must not silently accept an unknown path or destination")
}

type codexIntegrationStore struct {
	db    *sql.DB
	ent   *dbent.Client
	redis *redis.Client
}

func newCodexIntegrationStore(t *testing.T) *codexIntegrationStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pg, err := tcpostgres.Run(ctx, "postgres:18.1-alpine3.23", tcpostgres.WithDatabase("codex_gateway_test"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("synthetic-test-password"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err, "PostgreSQL testcontainer is required")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		require.NoError(t, pg.Terminate(cleanup))
	})
	rc, err := tcredis.Run(ctx, "redis:8.4-alpine")
	require.NoError(t, err, "Redis testcontainer is required")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		require.NoError(t, rc.Terminate(cleanup))
	})
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.PingContext(ctx))
	require.NoError(t, repository.ApplyMigrations(ctx, db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	host, err := rc.Host(ctx)
	require.NoError(t, err)
	port, err := rc.MappedPort(ctx, "6379/tcp")
	require.NoError(t, err)
	rdb := redis.NewClient(&redis.Options{Addr: net.JoinHostPort(host, port.Port())})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	require.NoError(t, rdb.Ping(ctx).Err())
	return &codexIntegrationStore{db: db, ent: client, redis: rdb}
}

type codexIntegrationGateway struct {
	store          *codexIntegrationStore
	upstream       *codexIntegrationUpstream
	mode           string
	server         *httptest.Server
	gateway        *service.OpenAIGatewayService
	workers        *service.UsageRecordWorkerPool
	concurrency    *service.ConcurrencyService
	slots          service.ConcurrencyCache
	users          []*service.User
	keys           []*service.APIKey
	accounts       []*service.Account
	failoverKey    *service.APIKey
	rejectedKeys   []string
	expected       map[int64]int
	activeHandlers atomic.Int64
}

func newCodexIntegrationGateway(t *testing.T, store *codexIntegrationStore, upstream *codexIntegrationUpstream, mode string) *codexIntegrationGateway {
	t.Helper()
	ctx := context.Background()
	cfg := &config.Config{RunMode: "standard"}
	cfg.Default.RateMultiplier = 1
	cfg.APIKeyAuth.L1Size = 32
	cfg.APIKeyAuth.L1TTLSeconds = 30
	cfg.APIKeyAuth.L2TTLSeconds = 60
	cfg.APIKeyAuth.Singleflight = true
	cfg.APIKeyAuth.LookupConcurrency = 8
	cfg.Gateway.MaxBodySize, cfg.Gateway.TextMaxBodySize = 1<<20, 1<<20
	cfg.Gateway.MaxAccountSwitches = 3
	cfg.Gateway.Scheduling.DbFallbackEnabled = true
	cfg.Gateway.Scheduling.DbFallbackTimeoutSeconds = 5
	cfg.Gateway.Scheduling.OutboxPollIntervalSeconds = 1
	cfg.Gateway.Scheduling.FallbackSelectionMode = "last_used"
	cfg.Gateway.Scheduling.FallbackWaitTimeout = 2 * time.Second
	cfg.Gateway.Scheduling.FallbackMaxWaiting = 4
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 2 * time.Second
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 4
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	cfg.Gateway.OpenAIWS.Enabled = mode != "http"
	cfg.Gateway.OpenAIWS.OAuthEnabled = mode != "http"
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = mode
	cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
	cfg.Gateway.OpenAIWS.SessionDisableEnabled = true
	cfg.Gateway.OpenAIWS.StoreDisabledConnMode = "strict"
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 8
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.ClientFirstMessageTimeoutSeconds = 5
	cfg.Gateway.OpenAIWS.MaxIngressConnectionsPerAPIKey = 4
	disableTibo := false
	cfg.Gateway.OpenAITiboRoute.Enabled = &disableTibo
	schedulerCache := repository.NewSchedulerCache(store.redis)
	accounts := repository.NewAccountRepository(store.ent, store.db, schedulerCache)
	groups := repository.NewGroupRepository(store.ent, store.db)
	users := repository.NewUserRepository(store.ent, store.db)
	keys := repository.NewAPIKeyRepository(store.ent, store.db)
	subs := repository.NewUserSubscriptionRepository(store.ent)
	rates := repository.NewUserGroupRateRepository(store.db)
	usage := repository.NewUsageLogRepository(store.ent, store.db)
	usageBilling := repository.NewUsageBillingRepository(store.ent, store.db)
	proxyRepo := repository.NewProxyRepository(store.ent, store.db)
	settings := service.NewSettingService(repository.NewSettingRepository(store.ent), cfg)
	f := &codexIntegrationGateway{store: store, upstream: upstream, mode: mode, expected: make(map[int64]int)}
	group := &service.Group{Name: "integration-shared-" + mode, Platform: service.PlatformOpenAI, Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 2}
	require.NoError(t, groups.Create(ctx, group))
	failoverGroup := &service.Group{Name: "integration-failover-" + mode, Platform: service.PlatformOpenAI, Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 2}
	require.NoError(t, groups.Create(ctx, failoverGroup))
	proxyURL, err := url.Parse(upstream.proxy.URL)
	require.NoError(t, err)
	var proxyPort int
	_, err = fmt.Sscan(proxyURL.Port(), &proxyPort)
	require.NoError(t, err)
	proxy := &service.Proxy{Name: "integration-loopback-" + mode, Protocol: "http", Host: proxyURL.Hostname(), Port: proxyPort, Status: service.StatusActive}
	require.NoError(t, proxyRepo.Create(ctx, proxy))
	accountService := service.NewAccountService(accounts, groups)
	for i := range 3 {
		label := fmt.Sprintf("%s-account-%d", mode, i)
		groupIDs := []int64{failoverGroup.ID}
		if i != 1 {
			groupIDs = append(groupIDs, group.ID)
		}
		concurrency := 8
		a, err := accountService.Create(ctx, service.CreateAccountRequest{
			Name: label, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			Credentials: map[string]any{
				"access_token":       syntheticCodexIntegrationJWT(label),
				"refresh_token":      "synthetic-refresh-" + label,
				"chatgpt_account_id": "workspace-" + label,
				"chatgpt_user_id":    "owner-" + label,
				"expires_at":         float64(4102444800),
			},
			Extra: map[string]any{
				"openai_oauth_responses_websockets_v2_mode":    mode,
				"openai_oauth_responses_websockets_v2_enabled": mode != "http",
				"openai_passthrough":                           true,
				// Optional production tracing config, used for SQL/request correlation.
				service.AccountExtraUpstreamRequestIDHeader: "x-request-id",
			},
			ProxyID: &proxy.ID, Concurrency: &concurrency, Priority: i + 1, GroupIDs: groupIDs,
		})
		require.NoError(t, err)
		// Explicitly enable fixture accounts via the production repository API;
		// AccountService.Create itself leaves the schedulable flag disabled.
		if i < 2 {
			require.NoError(t, accounts.SetSchedulable(ctx, a.ID, true))
		}
		if i == 2 {
			status := service.StatusDisabled
			a, err = accountService.Update(ctx, a.ID, service.UpdateAccountRequest{Status: &status})
			require.NoError(t, err)
		}
		a, err = accounts.GetByID(ctx, a.ID)
		require.NoError(t, err)
		require.Regexp(t, `^[0-9a-f-]{36}$`, a.Extra["codex_fingerprint_seed"], "AccountService.Create must persist the default full fingerprint seed")
		require.Equal(t, "full", string(a.GetCodexFingerprintMode()))
		if i < 2 {
			require.True(t, a.Schedulable)
			require.Equal(t, service.StatusActive, a.Status)
		}
		upstream.registerAccount(a)
		f.accounts = append(f.accounts, a)
	}
	for i := range 3 {
		u := &service.User{Email: fmt.Sprintf("%s-%d@example.invalid", mode, i), PasswordHash: "synthetic-not-a-password", Role: service.RoleUser, Status: service.StatusActive, Balance: 100, Concurrency: 8}
		if i == 2 {
			u.Status = service.StatusDisabled
		}
		require.NoError(t, users.Create(ctx, u))
		k := &service.APIKey{UserID: u.ID, Key: fmt.Sprintf("sk-integration-%s-user-%d", mode, i), Name: "integration", GroupID: &group.ID, Status: service.StatusActive, Quota: 100, Concurrency: 4}
		require.NoError(t, keys.Create(ctx, k))
		f.users, f.keys = append(f.users, u), append(f.keys, k)
		if i == 2 {
			f.rejectedKeys = append(f.rejectedKeys, k.Key)
		}
	}
	disabledKey := &service.APIKey{UserID: f.users[0].ID, Key: "sk-integration-" + mode + "-disabled", Name: "disabled", GroupID: &group.ID, Status: service.StatusAPIKeyDisabled, Quota: 100}
	require.NoError(t, keys.Create(ctx, disabledKey))
	f.rejectedKeys = append(f.rejectedKeys, disabledKey.Key)
	f.failoverKey = &service.APIKey{UserID: f.users[0].ID, Key: "sk-integration-" + mode + "-failover", Name: "failover", GroupID: &failoverGroup.ID, Status: service.StatusActive, Quota: 100, Concurrency: 4}
	require.NoError(t, keys.Create(ctx, f.failoverKey))
	f.slots = repository.NewConcurrencyCache(store.redis, 1, 5)
	f.concurrency = service.NewConcurrencyService(f.slots)
	keyService := service.NewAPIKeyService(keys, users, groups, subs, rates, repository.NewAPIKeyCache(store.redis), cfg)
	keyService.SetConcurrencyService(f.concurrency)
	billingCache := service.NewBillingCacheService(repository.NewBillingCache(store.redis), users, subs, keys, repository.NewUserRPMCache(store.redis), rates, cfg, nil)
	t.Cleanup(billingCache.Stop)
	subscriptions := service.NewSubscriptionService(groups, subs, billingCache, store.ent, cfg)
	t.Cleanup(subscriptions.Stop)
	oauth := service.NewOpenAIOAuthService(proxyRepo, repository.NewOpenAIOAuthClient())
	t.Cleanup(oauth.Stop)
	tokenCache := repository.NewTokenCache(store.redis)
	tokenProvider := service.NewOpenAITokenProvider(accounts, tokenCache, oauth)
	tokenProvider.SetRefreshAPI(service.NewOAuthRefreshAPI(accounts, tokenCache), service.NewOpenAITokenRefresher(oauth, accounts))
	snapshot := service.NewSchedulerSnapshotService(schedulerCache, repository.NewSchedulerOutboxRepository(store.db), accounts, groups, cfg)
	snapshot.Start()
	t.Cleanup(snapshot.Stop)
	timingWheel, err := service.NewTimingWheelService()
	require.NoError(t, err)
	t.Cleanup(timingWheel.Stop)
	deferred := service.NewDeferredService(accounts, timingWheel, time.Minute)
	deferred.Start()
	t.Cleanup(deferred.Stop)
	rateLimit := service.NewRateLimitService(accounts, usage, cfg, repository.NewTempUnschedCache(store.redis))
	rateLimit.SetSettingService(settings)
	f.gateway = service.NewOpenAIGatewayService(accounts, usage, usageBilling, users, subs, rates, repository.NewGatewayCache(store.redis), cfg, snapshot, f.concurrency, service.NewBillingService(cfg, nil), rateLimit, billingCache, repository.NewHTTPUpstream(cfg), deferred, tokenProvider, nil, nil, nil, nil, settings, nil)
	t.Cleanup(func() { f.gateway.StopOpenAICodexTicketHarvester(); f.gateway.CloseOpenAIWSPool() })
	f.workers = service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 2, QueueSize: 32, TaskTimeout: 10 * time.Second, OverflowPolicy: config.UsageRecordOverflowPolicySync})
	t.Cleanup(f.workers.Stop)
	h := handler.NewOpenAIGatewayHandler(f.gateway, f.concurrency, billingCache, keyService, f.workers, nil, nil, nil, cfg)
	router := gin.New()
	routes.RegisterGatewayRoutes(router, &handler.Handlers{OpenAIGateway: h, Gateway: &handler.GatewayHandler{}, AsyncImage: &handler.AsyncImageHandler{}}, middleware.NewAPIKeyAuthMiddleware(keyService, subscriptions, cfg), keyService, subscriptions, nil, settings, nil, cfg)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.activeHandlers.Add(1)
		defer f.activeHandlers.Add(-1)
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func codexIntegrationPayload(marker string, ws bool) []byte {
	body := map[string]any{"model": "gpt-5.2", "instructions": "Reply with OK.", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": marker}}}}, "store": false, "stream": true, "prompt_cache_key": "same-raw-cache"}
	body["client_metadata"] = map[string]any{"session_id": "same-raw-session", "thread_id": "same-raw-thread", "turn_id": "same-raw-turn"}
	if ws {
		body["type"] = "response.create"
		delete(body, "stream")
	}
	data, _ := json.Marshal(body)
	return data
}

func codexIntegrationHeaders(key string) http.Header {
	h := make(http.Header)
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", service.CodexCanonicalUserAgent())
	h.Set("session_id", "same-raw-session")
	h.Set("x-codex-thread-id", "same-raw-thread")
	h.Set("x-codex-turn-id", "same-raw-turn")
	return h
}

func (f *codexIntegrationGateway) post(ctx context.Context, key, marker, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+path, bytes.NewReader(codexIntegrationPayload(marker, false)))
	if err != nil {
		return nil, err
	}
	req.Header = codexIntegrationHeaders(key)
	return (&http.Client{Timeout: 15 * time.Second}).Do(req)
}

func (f *codexIntegrationGateway) testRejection(t *testing.T) {
	before := f.upstream.requestCount()
	connects := f.upstream.connectCount()
	for _, key := range append([]string{"", "sk-integration-invalid"}, f.rejectedKeys...) {
		resp, err := f.post(context.Background(), key, f.mode+"-rejected", "/v1/responses")
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, resp.StatusCode)
	}
	require.Equal(t, before, f.upstream.requestCount())
	require.Equal(t, connects, f.upstream.connectCount(), "rejected auth must not even open an upstream connection")
	for _, key := range f.keys {
		require.Empty(t, f.rows(t, key.ID))
	}
	for _, user := range f.users {
		var balance float64
		require.NoError(t, f.store.db.QueryRow("SELECT balance FROM users WHERE id=$1", user.ID).Scan(&balance))
		require.Equal(t, float64(100), balance)
	}
}

func (f *codexIntegrationGateway) completedHTTP(t *testing.T, key *service.APIKey, marker, path string) {
	t.Helper()
	resp, err := f.post(context.Background(), key.Key, marker, path)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)
	require.Contains(t, string(body), `"type":"response.completed"`)
	f.expected[key.ID]++
	f.waitRows(t, key.ID, f.expected[key.ID])
}

func (f *codexIntegrationGateway) testHTTPComplete(t *testing.T) {
	f.completedHTTP(t, f.keys[0], "http-complete", "/v1/responses")
	f.completedHTTP(t, f.keys[0], "http-duplicate-terminal", "/backend-api/codex/responses")
	require.Len(t, f.upstream.requestsFor("http-complete"), 1)
	require.Len(t, f.upstream.requestsFor("http-duplicate-terminal"), 1)
}

func (f *codexIntegrationGateway) testHTTPIsolation(t *testing.T) {
	results := make(chan error, 2)
	for i, marker := range []string{"http-isolation-a", "http-isolation-b"} {
		go func(key *service.APIKey, marker string) {
			resp, err := f.post(context.Background(), key.Key, marker, "/v1/responses")
			if err != nil {
				results <- err
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err == nil && (resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"type":"response.completed"`)) {
				err = fmt.Errorf("concurrent HTTP status=%d body=%s", resp.StatusCode, body)
			}
			results <- err
		}(f.keys[i], marker)
	}
	for range 2 {
		require.NoError(t, <-results)
	}
	for _, key := range f.keys[:2] {
		f.expected[key.ID]++
		f.waitRows(t, key.ID, f.expected[key.ID])
	}
	f.assertIsolation(t, "http-isolation-a", "http-isolation-b")
}

func (f *codexIntegrationGateway) testHTTPCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := f.post(ctx, f.keys[0].Key, "http-cancel", "/v1/responses")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	scanner := bufio.NewScanner(resp.Body)
	seenUsage := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), `"output_tokens":20`) {
			seenUsage = true
			break
		}
	}
	require.True(t, seenUsage, "cancel only after progressive usage reaches the real downstream socket")
	cancel()
	require.NoError(t, resp.Body.Close())
	f.upstream.finishCancel()
	f.expected[f.keys[0].ID]++
	f.waitRows(t, f.keys[0].ID, f.expected[f.keys[0].ID])
	require.Len(t, f.upstream.requestsFor("http-cancel"), 1, "semantic output must not be retried on another account")
}

func (f *codexIntegrationGateway) testHTTPSoft429(t *testing.T) {
	f.completedHTTP(t, f.keys[0], "http-soft-429", "/v1/responses")
	records := f.upstream.requestsFor("http-soft-429")
	require.Len(t, records, 2, "synthetic soft 429 must be retried, not bypassed")
	require.Equal(t, records[0].accountID, records[1].accountID)
}

func (f *codexIntegrationGateway) testHTTPFailover(t *testing.T) {
	f.completedHTTP(t, f.failoverKey, "http-failover", "/v1/responses")
	records := f.upstream.requestsFor("http-failover")
	require.GreaterOrEqual(t, len(records), 2)
	first, last := records[0], records[len(records)-1]
	require.NotEqual(t, first.accountID, last.accountID, "the real scheduler must switch accounts")
	require.Contains(t, []int64{f.accounts[0].ID, f.accounts[1].ID}, last.accountID)
	require.NotEmpty(t, first.header.Get("session-id"))
	require.NotEqual(t, first.header.Get("session-id"), last.header.Get("session-id"))
	require.NotEqual(t, gjson.GetBytes(first.body, "prompt_cache_key").String(), gjson.GetBytes(last.body, "prompt_cache_key").String())
	require.Empty(t, gjson.GetBytes(last.body, "previous_response_id").String())
	require.Empty(t, last.header.Get("x-codex-turn-state"))
	rows := f.rows(t, f.failoverKey.ID)
	require.Len(t, rows, 1)
	require.Equal(t, last.accountID, rows[0].accountID)
}

func (f *codexIntegrationGateway) completedWS(t *testing.T, key *service.APIKey, marker string) {
	t.Helper()
	require.NoError(t, f.runWS(key, marker))
}

func (f *codexIntegrationGateway) runWS(key *service.APIKey, marker string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "http://", "ws://", 1)+"/backend-api/codex/responses", &websocket.DialOptions{HTTPHeader: codexIntegrationHeaders(key.Key)})
	if err != nil && resp != nil {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WS handshake status=%d body=%s: %w", resp.StatusCode, body, err)
	}
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	if err := conn.Write(ctx, websocket.MessageText, codexIntegrationPayload(marker, true)); err != nil {
		return err
	}
	for {
		_, body, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		typeName := gjson.GetBytes(body, "type").String()
		if typeName == "error" || typeName == "response.failed" {
			return fmt.Errorf("WS response: %s", body)
		}
		if typeName == "response.completed" {
			break
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "integration complete")
	return nil
}

func (f *codexIntegrationGateway) testWSComplete(t *testing.T) {
	before := f.gateway.SnapshotOpenAIWSPoolMetrics()
	marker := f.mode + "-complete"
	f.completedWS(t, f.keys[0], marker)
	f.expected[f.keys[0].ID]++
	f.waitRows(t, f.keys[0].ID, f.expected[f.keys[0].ID])
	records := f.upstream.requestsFor(marker)
	require.Len(t, records, 1)
	require.True(t, records[0].websocket)
	after := f.gateway.SnapshotOpenAIWSPoolMetrics()
	if f.mode == "ctx_pool" {
		require.Greater(t, after.AcquireTotal, before.AcquireTotal)
	} else {
		require.Equal(t, before.AcquireTotal, after.AcquireTotal, "passthrough must not be silently replaced by ctx_pool")
	}
}

func (f *codexIntegrationGateway) testWSIsolation(t *testing.T) {
	a, b := f.mode+"-isolation-a", f.mode+"-isolation-b"
	results := make(chan error, 2)
	go func() { results <- f.runWS(f.keys[0], a) }()
	go func() { results <- f.runWS(f.keys[1], b) }()
	for range 2 {
		require.NoError(t, <-results)
	}
	for _, key := range f.keys[:2] {
		f.expected[key.ID]++
		f.waitRows(t, key.ID, f.expected[key.ID])
	}
	f.assertIsolation(t, a, b)
}

func (f *codexIntegrationGateway) testWSReplay(t *testing.T) {
	marker := f.mode + "-replay"
	f.completedWS(t, f.keys[0], marker)
	f.expected[f.keys[0].ID]++
	f.waitRows(t, f.keys[0].ID, f.expected[f.keys[0].ID])
	f.completedWS(t, f.keys[0], marker)
	// The same response ID is a legitimate dedup key only within the same API key.
	f.completedWS(t, f.keys[1], marker)
	f.expected[f.keys[1].ID]++
	f.waitRows(t, f.keys[1].ID, f.expected[f.keys[1].ID])
	f.drain(t)
	require.Len(t, f.upstream.requestsFor(marker), 3, "replay must actually traverse the gateway three times")
	require.Len(t, f.rows(t, f.keys[0].ID), f.expected[f.keys[0].ID])
	for _, key := range f.keys[:2] {
		var count int
		require.NoError(t, f.store.db.QueryRow("SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=$1 AND request_id=$2", key.ID, "resp_"+marker).Scan(&count))
		require.Equal(t, 1, count)
	}
}

func (f *codexIntegrationGateway) assertIsolation(t *testing.T, markerA, markerB string) {
	t.Helper()
	a, b := f.upstream.requestsFor(markerA), f.upstream.requestsFor(markerB)
	require.Len(t, a, 1)
	require.Len(t, b, 1)
	require.Equal(t, f.accounts[0].ID, a[0].accountID)
	require.Equal(t, a[0].accountID, b[0].accountID, "test must share the upstream owner, not merely different accounts")
	require.NotEmpty(t, a[0].header.Get("session-id"))
	require.NotEqual(t, a[0].header.Get("session-id"), b[0].header.Get("session-id"))
	require.NotEmpty(t, gjson.GetBytes(a[0].body, "prompt_cache_key").String())
	require.NotEqual(t, gjson.GetBytes(a[0].body, "prompt_cache_key").String(), gjson.GetBytes(b[0].body, "prompt_cache_key").String())
	for _, field := range []string{"session_id", "thread_id", "turn_id"} {
		av, bv := gjson.GetBytes(a[0].body, "client_metadata."+field).String(), gjson.GetBytes(b[0].body, "client_metadata."+field).String()
		require.NotEmpty(t, av, field)
		require.NotEqual(t, av, bv, field+" must be scoped to the authenticated API key")
	}
	for _, r := range []codexIntegrationRequest{a[0], b[0]} {
		require.Empty(t, r.header.Get("x-codex-turn-state"))
		require.Empty(t, gjson.GetBytes(r.body, "client_metadata.x-codex-turn-state").String())
		require.Empty(t, gjson.GetBytes(r.body, "previous_response_id").String())
	}
}

type codexIntegrationUsage struct {
	userID, keyID, accountID     int64
	requestID, upstreamID, model string
	input, output                int
	total, actual                float64
}

func (f *codexIntegrationGateway) rows(t *testing.T, keyID int64) []codexIntegrationUsage {
	t.Helper()
	rows, err := f.store.db.Query("SELECT user_id, api_key_id, account_id, request_id, COALESCE(upstream_request_id,''), model, input_tokens, output_tokens, total_cost, actual_cost FROM usage_logs WHERE api_key_id=$1 ORDER BY id", keyID)
	require.NoError(t, err)
	defer rows.Close()
	var result []codexIntegrationUsage
	for rows.Next() {
		var r codexIntegrationUsage
		require.NoError(t, rows.Scan(&r.userID, &r.keyID, &r.accountID, &r.requestID, &r.upstreamID, &r.model, &r.input, &r.output, &r.total, &r.actual))
		result = append(result, r)
	}
	require.NoError(t, rows.Err())
	return result
}

func (f *codexIntegrationGateway) waitRows(t *testing.T, keyID int64, count int) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		err := f.store.db.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", keyID).Scan(&n)
		return err == nil && n >= count
	}, 10*time.Second, 20*time.Millisecond, "usage must persist after request completion/cancellation")
	f.drain(t)
	require.Len(t, f.rows(t, keyID), count)
}

func (f *codexIntegrationGateway) drain(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return f.activeHandlers.Load() == 0 }, 10*time.Second, 20*time.Millisecond, "handlers must finish submitting usage before draining the worker pool")
	require.Eventually(t, func() bool {
		s := f.workers.Stats()
		return s.CompletedTasks+s.SyncFallbackTasks >= s.SubmittedTasks && s.WaitingTasks == 0 && s.RunningWorkers == 0
	}, 10*time.Second, 20*time.Millisecond, "usage workers must finish before ledger assertions")
	require.Zero(t, f.workers.Stats().FailedTasks)
	require.Zero(t, f.workers.Stats().DroppedTasks)
}

func (f *codexIntegrationGateway) testLedgerAndSlots(t *testing.T) {
	f.drain(t)
	// Production's built-in gpt-5.2 prices: 100*$1.75/M + 20*$14/M.
	// Group multiplier is 2. No live pricing source is started by this test.
	const total = 0.000455
	const actual = 0.00091
	userCharges := make(map[int64]float64)
	allKeys := append(append([]*service.APIKey{}, f.keys...), f.failoverKey)
	for _, key := range allKeys {
		rows := f.rows(t, key.ID)
		require.Len(t, rows, f.expected[key.ID])
		for _, row := range rows {
			require.Equal(t, key.UserID, row.userID)
			require.Equal(t, key.ID, row.keyID)
			require.Contains(t, []int64{f.accounts[0].ID, f.accounts[1].ID}, row.accountID)
			require.NotEmpty(t, row.requestID)
			marker := strings.TrimPrefix(row.requestID, "resp_")
			if f.mode == "http" {
				require.True(t, strings.HasPrefix(row.requestID, "client:"), "HTTP billing uses the generated ingress request ID")
				require.True(t, strings.HasPrefix(row.upstreamID, "request_http-"), "persist the actually observed upstream request ID")
				marker = strings.TrimPrefix(row.upstreamID, "request_")
			}
			observed := f.upstream.requestsFor(marker)
			require.NotEmpty(t, observed, "every ledger row must correlate to an actual local upstream request")
			require.Equal(t, observed[len(observed)-1].accountID, row.accountID)
			require.Equal(t, "gpt-5.2", row.model)
			require.Equal(t, 100, row.input)
			require.Equal(t, 20, row.output)
			require.InDelta(t, total, row.total, 1e-9)
			require.InDelta(t, actual, row.actual, 1e-9)
			userCharges[key.UserID] += actual
		}
		var quota float64
		require.NoError(t, f.store.db.QueryRow("SELECT quota_used FROM api_keys WHERE id=$1", key.ID).Scan(&quota))
		require.InDelta(t, float64(len(rows))*actual, quota, 1e-8)
		var dedup int
		require.NoError(t, f.store.db.QueryRow("SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=$1", key.ID).Scan(&dedup))
		require.Equal(t, len(rows), dedup)
	}
	for _, user := range f.users {
		var balance float64
		require.NoError(t, f.store.db.QueryRow("SELECT balance FROM users WHERE id=$1", user.ID).Scan(&balance))
		require.InDelta(t, 100-userCharges[user.ID], balance, 1e-8)
	}
	require.Eventually(t, func() bool {
		ctx := context.Background()
		for _, user := range f.users {
			n, err := f.slots.GetUserConcurrency(ctx, user.ID)
			if err != nil || n != 0 {
				return false
			}
		}
		ids := []int64{f.accounts[0].ID, f.accounts[1].ID, f.accounts[2].ID}
		counts, err := f.concurrency.GetAccountConcurrencyBatch(ctx, ids)
		if err != nil {
			return false
		}
		for _, n := range counts {
			if n != 0 {
				return false
			}
		}
		keyIDs := []int64{f.keys[0].ID, f.keys[1].ID, f.failoverKey.ID}
		counts, err = f.concurrency.GetAPIKeyConcurrencyBatch(ctx, keyIDs)
		if err != nil {
			return false
		}
		for _, n := range counts {
			if n != 0 {
				return false
			}
		}
		return true
	}, 10*time.Second, 20*time.Millisecond, "all user/key/account concurrency slots must release")
	for _, r := range f.upstream.requestsWithPrefix(f.mode + "-") {
		require.NotEqual(t, f.accounts[2].ID, r.accountID, "disabled account must never be scheduled")
	}
	t.Logf("verified mode=%s requests=%d usage_rows=%d workers=%+v", f.mode, len(f.upstream.requestsWithPrefix(f.mode+"-")), f.expected[f.keys[0].ID]+f.expected[f.keys[1].ID]+f.expected[f.failoverKey.ID], f.workers.Stats())
}
