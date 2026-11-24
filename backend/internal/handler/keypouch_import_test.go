package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

var testKeyPouchApp = openai.KeyPouchRegistration{ClientID: "test-owner-client", RedirectURI: "http://127.0.0.1:31872/keypouch/oauth/callback"}

func TestKeyPouchRegistration(t *testing.T) {
	cfg := config.OIDCConnectConfig{Enabled: true, ClientID: testKeyPouchApp.ClientID, RedirectURL: testKeyPouchApp.RedirectURI, IssuerURL: openai.RegisteredIssuer, AuthorizeURL: openai.RegisteredAuthorizeURL, TokenURL: openai.RegisteredTokenURL, UsePKCE: true, TokenAuthMethod: "none", Scopes: openai.DefaultScopes}
	app, err := keyPouchExistingRegistration(cfg)
	require.NoError(t, err)
	require.Equal(t, testKeyPouchApp, app)
	for _, edit := range []func(*config.OIDCConnectConfig){
		func(c *config.OIDCConnectConfig) { c.ClientID = openai.ClientID }, func(c *config.OIDCConnectConfig) { c.RedirectURL = openai.DefaultRedirectURI },
		func(c *config.OIDCConnectConfig) { c.Scopes = "openid" }, func(c *config.OIDCConnectConfig) { c.TokenAuthMethod = "client_secret_post" },
		func(c *config.OIDCConnectConfig) { c.IssuerURL = "https://issuer.invalid" }, func(c *config.OIDCConnectConfig) { c.JWKSURL = "https://jwks.invalid" },
	} {
		wrong := cfg
		edit(&wrong)
		_, err := keyPouchExistingRegistration(wrong)
		require.ErrorIs(t, err, service.KeyPouchOAuthMissing)
	}
	for _, uri := range []string{"http://localhost:31872/keypouch/oauth/callback", "http://127.0.0.1/keypouch/oauth/callback", "https://127.0.0.1:31872/keypouch/oauth/callback", "http://user@127.0.0.1:31872/keypouch/oauth/callback", testKeyPouchApp.RedirectURI + "?", testKeyPouchApp.RedirectURI + "#"} {
		require.False(t, openai.KeyPouchRegistration{ClientID: "test", RedirectURI: uri}.Valid())
	}
	require.True(t, openai.KeyPouchRegistration{ClientID: "test", RedirectURI: "http://[::1]:4444/keypouch/oauth/callback"}.Valid())
}

func TestKeyPouchHTTPBoundary(t *testing.T) {
	router := gin.New()
	h := &KeyPouchImportHandler{secret: "test-local-secret"}
	router.POST("/imports/:job_id/start", h.Start)
	try := func(remote, host, auth, header, body string) (int, string) {
		r := httptest.NewRequest("POST", "http://"+host+"/imports/0123/start", strings.NewReader(body))
		r.RemoteAddr = remote
		r.Header.Set("Content-Type", "application/json")
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if header != "" {
			r.Header.Set(header, "value")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		return w.Code, w.Body.String()
	}
	for _, check := range []struct {
		remote, host, auth, header string
		want                       int
	}{
		{"192.0.2.2:1234", "127.0.0.1:8080", "Bearer test-local-secret", "", 404},
		{"127.0.0.1:1234", "public.example", "Bearer test-local-secret", "", 404},
		{"127.0.0.1:1234", "127.0.0.1:8080", "Bearer test-local-secret", "X-Forwarded-For", 404},
		{"127.0.0.1:1234", "127.0.0.1:8080", "Bearer test-local-secret", "Origin", 404},
		{"127.0.0.1:1234", "127.0.0.1:8080", "Bearer wrong", "", 401},
		{"127.0.0.1:1234", "127.0.0.1:8080", "Bearer test-local-secret", "", 400},
	} {
		status, body := try(check.remote, check.host, check.auth, check.header, `{"email":"x@example.invalid","token":"never-return"}`)
		require.Equal(t, check.want, status)
		require.NotContains(t, body, "never-return")
	}
	h.secret = ""
	status, _ := try("127.0.0.1:1234", "localhost:8080", "", "", `{}`)
	require.Equal(t, 503, status)
}

func keyPouchTestDB(t *testing.T) *sql.DB {
	t.Helper()
	socket := os.Getenv("SUB2API_TEST_KEYPOUCH_SOCKET")
	if socket == "" {
		t.Skip("set SUB2API_TEST_KEYPOUCH_SOCKET for a disposable PostgreSQL cluster")
	}
	require.True(t, strings.HasPrefix(socket, "/"))
	q := url.Values{"host": {socket}, "port": {os.Getenv("SUB2API_TEST_KEYPOUCH_PORT")}, "sslmode": {"disable"}}
	openDB := func(name string) *sql.DB {
		dsn := url.URL{Scheme: "postgres", Path: "/" + name, RawQuery: q.Encode()}
		db, err := sql.Open("postgres", dsn.String())
		require.NoError(t, err)
		return db
	}
	control := openDB("keypouch_import_test")
	t.Cleanup(func() { _ = control.Close() })
	name := fmt.Sprintf("keypouch_scratch_%d", time.Now().UnixNano())
	_, err := control.Exec("CREATE DATABASE " + name)
	require.NoError(t, err)
	t.Cleanup(func() { _, e := control.Exec("DROP DATABASE " + name); require.NoError(t, e) })
	db := openDB(name)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, repository.ApplyMigrations(context.Background(), db))
	return db
}

func TestKeyPouchPostgres(t *testing.T) {
	db := keyPouchTestDB(t)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("34", 32)
	cipher, err := repository.NewAESEncryptor(cfg)
	require.NoError(t, err)
	admin := service.NewAdminService(cfg, nil, repository.NewAdminGroupRepository(client, db), repository.NewAdminAccountRepository(client, db, nil), nil, nil, nil, nil, nil, nil, nil, nil, nil, client, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	newHandler := func() *KeyPouchImportHandler {
		h := NewKeyPouchImportHandler(repository.NewKeyPouchImportStore(client), admin, service.NewOpenAIOAuthService(nil, nil), cipher, nil)
		h.registration = func(context.Context) (openai.KeyPouchRegistration, error) { return testKeyPouchApp, nil }
		return h
	}
	h := newHandler()
	ctx := context.Background()
	scalar := func(q string, args ...any) int64 {
		t.Helper()
		var n int64
		require.NoError(t, db.QueryRow(q, args...).Scan(&n))
		return n
	}
	exec := func(q string, args ...any) { t.Helper(); _, err := db.Exec(q, args...); require.NoError(t, err) }
	group := scalar(`INSERT INTO groups(name,platform,status) VALUES('GPT-不降智','openai','active') RETURNING id`)
	other := scalar(`INSERT INTO groups(name,platform,status) VALUES('KeyPouch other','openai','active') RETURNING id`)
	insert := func(email, workspace, user string) int64 {
		b, err := json.Marshal(map[string]string{"email": email, "chatgpt_account_id": workspace, "chatgpt_user_id": user})
		require.NoError(t, err)
		return scalar(`INSERT INTO accounts(name,platform,type,credentials,status,schedulable,priority) VALUES($1,'openai','oauth',$2,'disabled',false,41) RETURNING id`, email, b)
	}
	t.Run("group additive and read back", func(t *testing.T) {
		id := insert("existing@example.invalid", "ws", "user")
		exec(`INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,7)`, id, other)
		onlyExisting := newHandler()
		onlyExisting.registration = func(context.Context) (openai.KeyPouchRegistration, error) {
			return openai.KeyPouchRegistration{}, service.KeyPouchOAuthMissing
		}
		member, e := onlyExisting.start(ctx, "existing", "existing@example.invalid")
		require.NoError(t, e)
		require.Equal(t, keyPouchResult{Status: "member", AccountID: id, GroupID: group}, member)
		require.EqualValues(t, 1, scalar(`SELECT count(*) FROM accounts WHERE id=$1 AND status='disabled' AND NOT schedulable AND priority=41`, id))
		require.EqualValues(t, 7, scalar(`SELECT priority FROM account_groups WHERE account_id=$1 AND group_id=$2`, id, other))
		require.EqualValues(t, 1, scalar(`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1 AND event_type=$2`, id, service.SchedulerOutboxEventAccountGroupsChanged))
		replay, e := h.complete(ctx, "existing", keyPouchCallback{"existing@example.invalid", "already-used", "used"})
		require.NoError(t, e)
		require.Equal(t, member, replay)
		_, e = h.start(ctx, "existing", "wrong@example.invalid")
		require.ErrorIs(t, e, service.KeyPouchEmailConflict)
		_, e = onlyExisting.start(ctx, "missing", "missing@example.invalid")
		require.ErrorIs(t, e, service.KeyPouchOAuthMissing)
		exec(`DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`, id, group)
		_, e = h.start(ctx, "existing", "existing@example.invalid")
		require.ErrorIs(t, e, service.KeyPouchMembershipChanged)
	})
	t.Run("ambiguous workspace", func(t *testing.T) {
		insert("ambiguous@example.invalid", "ws1", "user2")
		insert("ambiguous@example.invalid", "ws2", "user2")
		_, e := h.start(ctx, "ambiguous", "ambiguous@example.invalid")
		require.ErrorIs(t, e, service.KeyPouchIdentityConflict)
		require.Zero(t, scalar(`SELECT count(*) FROM account_groups g JOIN accounts a ON a.id=g.account_id WHERE a.name='ambiguous@example.invalid'`))
	})
	t.Run("legacy token-only account blocks duplicate", func(t *testing.T) {
		const address = "historic@example.invalid"
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/profile":{"email":"historic@example.invalid"}}`))
		b, e := json.Marshal(map[string]string{"access_token": "e30." + payload + ".fixture"})
		require.NoError(t, e)
		exec(`INSERT INTO accounts(name,platform,type,credentials) VALUES('Unrelated display label','openai','oauth',$1)`, b)
		_, e = h.start(ctx, "legacy-email", address)
		require.ErrorIs(t, e, service.KeyPouchIdentityConflict)
		// The ambiguous legacy claim cannot be hidden by a well-formed duplicate.
		insert(address, "historical-ws", "historical-user")
		_, e = h.start(ctx, "legacy-and-indexed", address)
		require.ErrorIs(t, e, service.KeyPouchIdentityConflict)
		require.Zero(t, scalar(`SELECT count(*) FROM account_groups ag JOIN accounts a ON a.id=ag.account_id WHERE a.name IN ($1,'Unrelated display label')`, address))
	})
	t.Run("provider consent and same-email races", func(t *testing.T) {
		const email = "new@example.invalid"
		priv, e := rsa.GenerateKey(rand.Reader, 2048)
		require.NoError(t, e)
		keys := oidcJWKSet{Keys: []oidcJWK{buildRSAJWK("test-kid", &priv.PublicKey)}}
		jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(keys) }))
		defer jwks.Close()
		var calls atomic.Int32
		var nonceForCode sync.Map
		var both sync.WaitGroup
		both.Add(2)
		setup := func(h *KeyPouchImportHandler) {
			h.verify = func(ctx context.Context, cfg config.OIDCConnectConfig, id, nonce string) (*oidcIDTokenClaims, error) {
				require.Equal(t, openai.RegisteredJWKSURL, cfg.JWKSURL)
				cfg.JWKSURL = jwks.URL
				return oidcParseAndValidateIDToken(ctx, cfg, id, nonce)
			}
			h.exchange = func(ctx context.Context, app openai.KeyPouchRegistration, code, verifier string) (*openai.TokenResponse, error) {
				calls.Add(1)
				value, ok := nonceForCode.Load(code)
				if !ok {
					return nil, errors.New("unknown synthetic consent")
				}
				data := value.([2]string)
				if data[1] != openai.GenerateCodeChallenge(verifier) {
					return nil, errors.New("challenge mismatch")
				}
				subjectEmail, workspace, accountUser := email, "shared", "upstream-user"
				if code == "mismatch" {
					subjectEmail = "other@example.invalid"
				}
				if code == "different-workspace" {
					workspace = "other"
				}
				if code == "race-one" || code == "race-two" {
					both.Done()
					both.Wait()
				}
				token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": openai.RegisteredIssuer, "aud": app.ClientID, "sub": "provider-subject",
					"exp": time.Now().Add(time.Hour).Unix(), "email": subjectEmail, "email_verified": true, "nonce": data[0],
					"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": workspace, "chatgpt_user_id": accountUser}})
				token.Header["kid"] = "test-kid"
				signed, e := token.SignedString(priv)
				if e != nil {
					return nil, e
				}
				return &openai.TokenResponse{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", IDToken: signed, TokenType: "Bearer", ExpiresIn: 3600}, nil
			}
		}
		setup(h)
		start := func(job, code string) keyPouchCallback {
			res, e := h.start(ctx, job, email)
			require.NoError(t, e)
			require.Equal(t, "authorization_required", res.Status)
			u, e := url.Parse(res.URL)
			require.NoError(t, e)
			q := u.Query()
			require.Equal(t, "S256", q.Get("code_challenge_method"))
			require.Equal(t, testKeyPouchApp.RedirectURI, q.Get("redirect_uri"))
			nonceForCode.Store(code, [2]string{q.Get("nonce"), q.Get("code_challenge")})
			return keyPouchCallback{Email: email, Code: code, State: q.Get("state")}
		}
		bad := start("consent", "mismatch")
		require.Equal(t, bad, start("consent", "mismatch"))
		restarted := newHandler()
		setup(restarted)
		untrusted := bad
		untrusted.State = "different"
		_, e = restarted.complete(ctx, "consent", untrusted)
		require.ErrorIs(t, e, service.KeyPouchStateMismatch)
		require.Zero(t, calls.Load())
		require.Zero(t, scalar(`SELECT count(*) FROM keypouch_import_jobs WHERE job_id='consent' AND position($1 in sealed_session)>0`, bad.State))
		_, e = restarted.complete(ctx, "consent", bad)
		require.ErrorIs(t, e, service.KeyPouchIdentityMismatch)
		require.Zero(t, scalar(`SELECT count(*) FROM accounts WHERE name=$1`, email))
		one := start("one", "race-one")
		two := start("two", "race-two")
		conflict := start("third", "different-workspace")
		var wg sync.WaitGroup
		var results [2]keyPouchResult
		var errs [2]error
		for i, work := range []struct {
			id  string
			req keyPouchCallback
		}{{"one", one}, {"two", two}} {
			wg.Add(1)
			go func() { defer wg.Done(); results[i], errs[i] = restarted.complete(ctx, work.id, work.req) }()
		}
		wg.Wait()
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
		require.Equal(t, results[0], results[1])
		require.Equal(t, "member", results[0].Status)
		replay, e := h.start(ctx, "one", email)
		require.NoError(t, e)
		require.Equal(t, results[0], replay)
		before := calls.Load()
		replay, e = h.complete(ctx, "two", two)
		require.NoError(t, e)
		require.Equal(t, results[0], replay)
		require.Equal(t, before, calls.Load())
		_, e = restarted.complete(ctx, "third", conflict)
		require.ErrorIs(t, e, service.KeyPouchIdentityConflict)
		require.EqualValues(t, 1, scalar(`SELECT count(*) FROM accounts WHERE credentials->>'email'=$1`, email))
		require.EqualValues(t, 1, scalar(`SELECT count(*) FROM accounts WHERE credentials->>'oauth_client_registration'='keypouch' AND schedulable AND concurrency=$1`, service.DefaultOpenAIOAuthConcurrency))
		require.Zero(t, scalar(`SELECT count(*) FROM keypouch_import_jobs WHERE phase!='pending' AND (sealed_session<>'' OR attempt<>'')`))
	})
	t.Run("one claimant finishes after disconnect", func(t *testing.T) {
		const email = "disconnect@example.invalid"
		worker := newHandler()
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		worker.exchange = func(ctx context.Context, _ openai.KeyPouchRegistration, _, _ string) (*openai.TokenResponse, error) {
			calls.Add(1)
			close(entered)
			select {
			case <-release:
				return &openai.TokenResponse{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", IDToken: "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"disconnect-ws","chatgpt_user_id":"disconnect-user"}}`)) + ".fixture", TokenType: "Bearer", ExpiresIn: 3600}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		worker.verify = func(context.Context, config.OIDCConnectConfig, string, string) (*oidcIDTokenClaims, error) {
			yes := true
			return &oidcIDTokenClaims{RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}, Email: email, EmailVerified: &yes}, nil
		}
		res, e := worker.start(ctx, "disconnect", email)
		require.NoError(t, e)
		u, e := url.Parse(res.URL)
		require.NoError(t, e)
		req := keyPouchCallback{Email: email, Code: "synthetic", State: u.Query().Get("state")}
		caller, disconnect := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() { _, e := worker.complete(caller, "disconnect", req); result <- e }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("exchange did not start")
		}
		disconnect()
		_, pollErr := worker.start(ctx, "disconnect", email)
		_, duplicateErr := worker.complete(ctx, "disconnect", req)
		close(release)
		e = <-result
		require.ErrorIs(t, pollErr, service.KeyPouchExchanging)
		require.ErrorIs(t, duplicateErr, service.KeyPouchExchanging)
		require.NoError(t, e)
		member, e := newHandler().start(ctx, "disconnect", email)
		require.NoError(t, e)
		require.Equal(t, "member", member.Status)
		require.EqualValues(t, 1, calls.Load())
	})
	t.Run("expired sessions and inactive target", func(t *testing.T) {
		res, e := h.start(ctx, "exp", "expires@example.invalid")
		require.NoError(t, e)
		u, _ := url.Parse(res.URL)
		exec(`UPDATE keypouch_import_jobs SET expires_at=NOW()-INTERVAL '1 second' WHERE job_id='exp'`)
		_, e = h.complete(ctx, "exp", keyPouchCallback{"expires@example.invalid", "unused", u.Query().Get("state")})
		require.ErrorIs(t, e, service.KeyPouchExpired)
		next, e := h.start(ctx, "exp", "expires@example.invalid")
		require.NoError(t, e)
		require.NotEqual(t, res.URL, next.URL)
		exec(`UPDATE groups SET status='disabled' WHERE id=$1`, group)
		_, e = h.start(ctx, "no-group", "other@example.invalid")
		require.ErrorIs(t, e, service.KeyPouchGroupUnavailable)
	})
}
