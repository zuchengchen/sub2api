package service_test

// Constructor wiring evidence is separate from the traffic acceptance receipt.
// It uses the actual Redis repository implementation against miniredis only to
// prove dependency injection/encryption/settings wiring; real Redis semantics
// and concurrency are covered by the repository integration suite.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const nativeWiringSettingKey = "openai_native_reasoning_replay_config"

type nativeWiringSettingsRepository struct {
	mu          sync.Mutex
	values      map[string]string
	nativeReads []string
}

var _ service.SettingRepository = (*nativeWiringSettingsRepository)(nil)

func (r *nativeWiringSettingsRepository) Get(ctx context.Context, key string) (*service.Setting, error) {
	value, err := r.GetValue(ctx, key)
	if err != nil {
		return nil, err
	}
	return &service.Setting{Key: key, Value: value}, nil
}
func (r *nativeWiringSettingsRepository) GetValue(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if key == nativeWiringSettingKey {
		r.nativeReads = append(r.nativeReads, value)
	}
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}
func (r *nativeWiringSettingsRepository) Set(ctx context.Context, key, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}
func (r *nativeWiringSettingsRepository) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	result := map[string]string{}
	for _, key := range keys {
		value, err := r.GetValue(ctx, key)
		if errors.Is(err, service.ErrSettingNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return result, nil
}
func (r *nativeWiringSettingsRepository) SetMultiple(ctx context.Context, values map[string]string) error {
	for key, value := range values {
		if err := r.Set(ctx, key, value); err != nil {
			return err
		}
	}
	return nil
}
func (r *nativeWiringSettingsRepository) GetAll(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := map[string]string{}
	for key, value := range r.values {
		result[key] = value
	}
	return result, nil
}
func (r *nativeWiringSettingsRepository) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}
func (r *nativeWiringSettingsRepository) modesRead(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	raw := append([]string(nil), r.nativeReads...)
	r.mu.Unlock()
	var modes []string
	for _, value := range raw {
		var policy struct {
			Mode string `json:"mode"`
		}
		require.NoError(t, json.Unmarshal([]byte(value), &policy))
		modes = append(modes, policy.Mode)
	}
	return modes
}

// The transport/request/response fixture is shared with the independent Chat
// traffic suite; the service is created exclusively by the public production
// constructor and real repository cache/settings dependencies below.
func newNativeConstructorWiringHarness(t *testing.T) (*nativeTrafficHarness, *nativeTrafficHTTP) {
	t.Helper()
	h := &nativeTrafficHarness{t: t}
	h.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "synthetic read failure", 400)
			return
		}
		if r.Header.Get("Content-Encoding") == "zstd" {
			decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(8<<20))
			if err != nil {
				http.Error(w, "synthetic decoder failure", 400)
				return
			}
			body, err = decoder.DecodeAll(body, nil)
			decoder.Close()
			if err != nil {
				http.Error(w, "synthetic decode failure", 400)
				return
			}
		}
		if !json.Valid(body) {
			http.Error(w, "invalid synthetic request", 400)
			return
		}
		h.mu.Lock()
		round := len(h.rows) + 1
		output := nativeTrafficOutput(round)
		h.rows = append(h.rows, nativeTrafficRow{Status: 200, Body: body, Output: output, Remote: r.RemoteAddr, Terminal: true, BodyEncoding: r.Header.Get("Content-Encoding")})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		emit := func(value any) {
			wire := append([]byte("data: "), nativeTrafficJSON(value)...)
			wire = append(wire, '\n', '\n')
			for len(wire) > 0 {
				n := min(19, len(wire))
				if _, err := w.Write(wire[:n]); err != nil {
					return
				}
				wire = wire[n:]
				w.(http.Flusher).Flush()
			}
		}
		id := fmt.Sprintf("resp_wiring_%d", round)
		model := gjson.GetBytes(body, "model").String()
		emit(map[string]any{"type": "response.created", "response": map[string]any{"id": id, "model": model, "status": "in_progress"}})
		for index, item := range output {
			emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
			var native map[string]any
			_ = json.Unmarshal(item, &native)
			if native["type"] == "message" {
				emit(map[string]any{"type": "response.output_text.delta", "output_index": index, "content_index": 0, "delta": fmt.Sprintf("Inspecting synthetic step %d.", round)})
			}
			if native["type"] == "function_call" {
				emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": index, "delta": native["arguments"]})
			}
			emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
		}
		emit(map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "model": model, "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 101, "output_tokens": 17, "total_tokens": 118, "input_tokens_details": map[string]any{"cached_tokens": 31}}}})
	}))
	t.Cleanup(h.upstream.Close)
	origin, err := url.Parse(h.upstream.URL)
	require.NoError(t, err)
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != origin.Host {
			return nil, errors.New("constructor fixture rejected non-local dial")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	return h, &nativeTrafficHTTP{origin: origin, client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("constructor fixture forbids redirects") }}}
}

func nativeWiringConfig(secret string) *config.Config {
	cfg := &config.Config{}
	yes, no := true, false
	cfg.JWT.Secret = secret
	cfg.Gateway.CodexFullSynthesis = &yes
	cfg.Gateway.CodexWire.MapOrderJitter = &no
	cfg.Gateway.CodexWire.Profile = "auto"
	return cfg
}
func nativeWiringStartService(t *testing.T, h *nativeTrafficHarness, upstream *nativeTrafficHTTP, client *redis.Client, settingsRepo *nativeWiringSettingsRepository, cfg *config.Config, userIDs ...int64) {
	t.Helper()
	userID := int64(428)
	if len(userIDs) > 0 {
		userID = userIDs[0]
	}
	cache := repository.NewGatewayCache(client)
	settings := service.NewSettingService(settingsRepo, cfg)
	svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, cache, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, settings, nil)
	svc.StopOpenAICodexTicketHarvester()
	t.Cleanup(svc.CloseOpenAIWSPool)
	account := &service.Account{ID: 37001, Name: "synthetic-constructor-wiring", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 2,
		Credentials: map[string]any{"access_token": "synthetic-constructor-local-token", "chatgpt_account_id": "synthetic-constructor-upstream"},
		Extra:       map[string]any{service.OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "recent_samples": []any{map[string]any{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}}}, "codex_fingerprint_mode": "full", "codex_fingerprint_seed": "11111111-1111-4111-8111-111111111111"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "Chat ingress required", 404)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, "synthetic read failure", 400)
			return
		}
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		c.Set("api_key", &service.APIKey{ID: 794, UserID: userID, User: &service.User{ID: userID}})
		result, err := svc.ForwardAsChatCompletions(r.Context(), c, account, body, "", "")
		if err != nil && !c.Writer.Written() {
			c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "synthetic forwarding failure"}})
		}
		h.mu.Lock()
		h.routes++
		h.results = append(h.results, result)
		if err != nil {
			h.errors = append(h.errors, err.Error())
		}
		h.mu.Unlock()
	}))
	t.Cleanup(server.Close)
	h.server = server
}
func nativeWiringStoredEntries(t *testing.T, client *redis.Client) int {
	t.Helper()
	keys, err := client.Keys(context.Background(), "native_reasoning_replay:*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys, "production repository must publish native records")
	entries := 0
	for _, key := range keys {
		values, err := client.HGetAll(context.Background(), key).Result()
		require.NoError(t, err)
		for field, value := range values {
			if strings.HasPrefix(field, "v:") {
				entries++
				require.Greater(t, len(value), 65)
				require.NotContains(t, value, "opaque_reasoning_")
				require.NotContains(t, value, "Inspecting synthetic")
				require.NotContains(t, value, "lookup_temperature")
			}
		}
	}
	require.Positive(t, entries)
	return entries
}
func nativeWiringRows(h *nativeTrafficHarness) []nativeTrafficRow {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]nativeTrafficRow(nil), h.rows...)
}

func TestNativeReasoningReplayProductionConstructorWiring(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			mr := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			require.NoError(t, client.Ping(context.Background()).Err())
			repo := &nativeWiringSettingsRepository{values: map[string]string{nativeWiringSettingKey: `{"mode":"replay","user_ids":[428]}`}}
			h, upstream := newNativeConstructorWiringHarness(t)
			nativeWiringStartService(t, h, upstream, client, repo, nativeWiringConfig("synthetic-constructor-native-secret"))
			history := nativeTrafficInitialHistory()
			assistant, err := h.call(history, stream, "")
			require.NoError(t, err)
			require.Equal(t, 1, nativeWiringStoredEntries(t, client))
			history = nativeTrafficContinue(history, assistant)
			_, err = h.call(history, stream, "")
			require.NoError(t, err)
			rows := nativeWiringRows(h)
			require.Len(t, rows, 2)
			require.Equal(t, 0, nativeTrafficComparisonExit(rows[1].Body, rows[0].Output))
			require.Equal(t, 2, nativeWiringStoredEntries(t, client))
			require.Contains(t, repo.modesRead(t), "replay")
			h.mu.Lock()
			errorsSeen := append([]string(nil), h.errors...)
			routes := h.routes
			h.mu.Unlock()
			require.Empty(t, errorsSeen)
			require.Equal(t, 2, routes)
			t.Log("normal constructor -> SettingService reader -> repository GatewayCache -> JWT-derived cipher: two real Chat HTTP/SSE turns restored complete native output")
		})
	}
}

func TestNativeReasoningReplayObserveCommitsForFreshReplayService(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			mr := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			repo := &nativeWiringSettingsRepository{values: map[string]string{nativeWiringSettingKey: `{"mode":"observe","user_ids":[428]}`}}
			secret := "synthetic-constructor-observe-secret"
			h, upstream := newNativeConstructorWiringHarness(t)
			nativeWiringStartService(t, h, upstream, client, repo, nativeWiringConfig(secret))
			history := nativeTrafficInitialHistory()
			assistant, err := h.call(history, stream, "")
			require.NoError(t, err)
			require.Equal(t, 1, nativeWiringStoredEntries(t, client))
			history = nativeTrafficContinue(history, assistant)
			_, err = h.call(history, stream, "")
			require.NoError(t, err)
			rows := nativeWiringRows(h)
			require.Equal(t, 1, nativeTrafficComparisonExit(rows[1].Body, rows[0].Output), "observe must preserve the ordinary compatibility egress")
			require.Equal(t, 2, nativeWiringStoredEntries(t, client))
			require.NoError(t, repo.Set(context.Background(), nativeWiringSettingKey, `{"mode":"replay","user_ids":[428]}`))
			// A fresh production instance forces a real setting read without sleeping
			// through the policy TTL or reaching into private runtime/cipher fields.
			nativeWiringStartService(t, h, upstream, client, repo, nativeWiringConfig(secret))
			_, err = h.call(history, stream, "")
			require.NoError(t, err)
			rows = nativeWiringRows(h)
			require.Len(t, rows, 3)
			require.Equal(t, 0, nativeTrafficComparisonExit(rows[2].Body, rows[0].Output), "fresh replay service must decrypt and restore the observe instance's complete record")
			modes := repo.modesRead(t)
			require.GreaterOrEqual(t, len(modes), 2)
			require.Equal(t, "observe", modes[0])
			require.Equal(t, "replay", modes[len(modes)-1])
			// Changing only the server JWT secret makes the old encrypted cache
			// unreadable while ordinary forwarding remains available.
			nativeWiringStartService(t, h, upstream, client, repo, nativeWiringConfig("different-synthetic-jwt-secret"))
			_, err = h.call(history, stream, "")
			require.NoError(t, err)
			rows = nativeWiringRows(h)
			require.Len(t, rows, 4)
			require.Equal(t, 1, nativeTrafficComparisonExit(rows[3].Body, rows[0].Output), "the production envelope key must actually depend on cfg.JWT.Secret")
			nativeWiringStartService(t, h, upstream, client, repo, nativeWiringConfig(secret))
			_, err = h.call(history, stream, "")
			require.NoError(t, err)
			rows = nativeWiringRows(h)
			require.Len(t, rows, 5)
			require.Equal(t, 0, nativeTrafficComparisonExit(rows[4].Body, rows[0].Output), "restoring the original secret must read the unchanged original record again")
			h.mu.Lock()
			errorsSeen := append([]string(nil), h.errors...)
			h.mu.Unlock()
			require.Empty(t, errorsSeen)
			t.Log("observe saved complete encrypted state without replay; a fresh constructor with the same Redis/secret restored it, and a different secret safely missed")
		})
	}
}
