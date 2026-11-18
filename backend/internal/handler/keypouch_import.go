package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/mail"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const keyPouchExchangeLease = 2 * time.Minute

var keyPouchJobID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$`)

type KeyPouchImportHandler struct {
	store        service.KeyPouchImportStore
	admin        service.AdminService
	oauth        *service.OpenAIOAuthService
	cipher       service.SecretEncryptor
	secret       string
	registration func(context.Context) (openai.KeyPouchRegistration, error)
	exchange     func(context.Context, openai.KeyPouchRegistration, string, string) (*openai.TokenResponse, error)
	verify       func(context.Context, config.OIDCConnectConfig, string, string) (*oidcIDTokenClaims, error)
}

func NewKeyPouchImportHandler(store service.KeyPouchImportStore, admin service.AdminService, oauth *service.OpenAIOAuthService, cipher service.SecretEncryptor, settings *service.SettingService) *KeyPouchImportHandler {
	dedicated := openai.KeyPouchRegistration{ClientID: os.Getenv("SUB2API_KEYPOUCH_OAUTH_CLIENT_ID"), RedirectURI: os.Getenv("SUB2API_KEYPOUCH_OAUTH_REDIRECT_URI")}
	return &KeyPouchImportHandler{store: store, admin: admin, oauth: oauth, cipher: cipher, secret: os.Getenv("SUB2API_KEYPOUCH_IMPORT_SECRET"),
		registration: func(ctx context.Context) (openai.KeyPouchRegistration, error) {
			if dedicated.ClientID != "" || dedicated.RedirectURI != "" {
				if !dedicated.Valid() {
					return openai.KeyPouchRegistration{}, service.KeyPouchOAuthMissing
				}
				return dedicated, nil
			}
			cfg, err := settings.GetOIDCConnectOAuthConfig(ctx)
			if err != nil {
				return openai.KeyPouchRegistration{}, service.KeyPouchOAuthMissing
			}
			return keyPouchExistingRegistration(cfg)
		},
		exchange: func(ctx context.Context, app openai.KeyPouchRegistration, code, verifier string) (*openai.TokenResponse, error) {
			return app.Exchange(ctx, code, verifier)
		}, verify: oidcParseAndValidateIDToken,
	}
}

func keyPouchExistingRegistration(cfg config.OIDCConnectConfig) (openai.KeyPouchRegistration, error) {
	app := openai.KeyPouchRegistration{ClientID: cfg.ClientID, RedirectURI: cfg.RedirectURL}
	if !cfg.Enabled || !cfg.UsePKCE || cfg.TokenAuthMethod != "none" || cfg.IssuerURL != openai.RegisteredIssuer ||
		cfg.AuthorizeURL != openai.RegisteredAuthorizeURL || cfg.TokenURL != openai.RegisteredTokenURL ||
		(cfg.JWKSURL != "" && cfg.JWKSURL != openai.RegisteredJWKSURL) || !keyPouchScopes(cfg.Scopes) || !app.Valid() {
		return openai.KeyPouchRegistration{}, service.KeyPouchOAuthMissing
	}
	return app, nil
}

func keyPouchScopes(granted string) bool {
	scopes := strings.Fields(granted)
	for _, scope := range strings.Fields(openai.DefaultScopes) {
		if !slices.Contains(scopes, scope) {
			return false
		}
	}
	return true
}

type keyPouchResult struct {
	Status    string `json:"status"`
	URL       string `json:"authorization_url,omitempty"`
	AccountID int64  `json:"account_id,omitempty"`
	GroupID   int64  `json:"group_id,omitempty"`
}
type keyPouchCallback struct {
	Email string `json:"email"`
	Code  string `json:"code"`
	State string `json:"state"`
}
type keyPouchSession struct {
	App                    openai.KeyPouchRegistration
	State, Verifier, Nonce string
}

func keyPouchLocal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return false
	}
	host = r.Host
	if h, _, e := net.SplitHostPort(host); e == nil {
		host = h
	}
	if host != "localhost" && !net.ParseIP(strings.Trim(host, "[]")).IsLoopback() {
		return false
	}
	for name := range r.Header {
		h := strings.ToLower(name)
		if h == "forwarded" || strings.HasPrefix(h, "x-forwarded-") || h == "x-real-ip" || h == "cf-connecting-ip" || h == "origin" || strings.HasPrefix(h, "sec-fetch-") {
			return false
		}
	}
	return true
}

func (h *KeyPouchImportHandler) authorize(c *gin.Context) bool {
	c.Header("Cache-Control", "no-store")
	if !keyPouchLocal(c.Request) {
		c.AbortWithStatusJSON(404, gin.H{"error": "not_found"})
		return false
	}
	if strings.TrimSpace(h.secret) == "" {
		keyPouchRespondError(c, service.KeyPouchDisabled)
		return false
	}
	auth := c.Request.Header.Values("Authorization")
	if len(auth) != 1 {
		keyPouchRespondError(c, service.KeyPouchUnauthorized)
		return false
	}
	scheme, token, ok := strings.Cut(auth[0], " ")
	got, want := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(h.secret))
	if !ok || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		keyPouchRespondError(c, service.KeyPouchUnauthorized)
		return false
	}
	return true
}

func keyPouchDecode(c *gin.Context, dst any) bool {
	d := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10))
	d.DisallowUnknownFields()
	if c.ContentType() != "application/json" || c.Request.URL.RawQuery != "" || !keyPouchJobID.MatchString(c.Param("job_id")) || d.Decode(dst) != nil || d.Decode(new(any)) != io.EOF {
		keyPouchRespondError(c, service.KeyPouchBadRequest)
		return false
	}
	return true
}

func keyPouchEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	addr, err := mail.ParseAddress(email)
	if err != nil || len(email) > 254 || addr.Name != "" || addr.Address != email {
		return "", service.KeyPouchBadRequest
	}
	return email, nil
}

func keyPouchRespondError(c *gin.Context, err error) {
	var code service.KeyPouchError
	if !errors.As(err, &code) {
		code = service.KeyPouchUnavailable
	}
	status := http.StatusConflict
	switch code {
	case service.KeyPouchUnauthorized:
		status = http.StatusUnauthorized
	case service.KeyPouchUnknownJob:
		status = http.StatusNotFound
	case service.KeyPouchBadRequest, service.KeyPouchStateMismatch:
		status = http.StatusBadRequest
	case service.KeyPouchExpired:
		status = http.StatusGone
	case service.KeyPouchExchangeFailed:
		status = http.StatusBadGateway
	case service.KeyPouchDisabled, service.KeyPouchOAuthMissing, service.KeyPouchGroupUnavailable, service.KeyPouchUnavailable:
		status = http.StatusServiceUnavailable
	case service.KeyPouchExchanging:
		c.Header("Retry-After", "2")
	case service.KeyPouchGroupConflict, service.KeyPouchEmailConflict, service.KeyPouchIdentityConflict, service.KeyPouchIdentityMismatch, service.KeyPouchUnverified, service.KeyPouchMembershipChanged:
		status = http.StatusConflict
	default:
		code, status = service.KeyPouchUnavailable, http.StatusServiceUnavailable
	}
	c.AbortWithStatusJSON(status, gin.H{"error": string(code)})
}

func (h *KeyPouchImportHandler) Start(c *gin.Context) {
	if !h.authorize(c) {
		return
	}
	var req struct {
		Email string `json:"email"`
	}
	if !keyPouchDecode(c, &req) {
		return
	}
	email, err := keyPouchEmail(req.Email)
	if err != nil {
		keyPouchRespondError(c, err)
		return
	}
	res, err := h.start(c.Request.Context(), c.Param("job_id"), email)
	if err != nil {
		keyPouchRespondError(c, err)
		return
	}
	c.JSON(200, res)
}

func (h *KeyPouchImportHandler) Complete(c *gin.Context) {
	if !h.authorize(c) {
		return
	}
	var req keyPouchCallback
	if !keyPouchDecode(c, &req) {
		return
	}
	email, err := keyPouchEmail(req.Email)
	if err != nil || req.Code == "" || len(req.Code) > 8192 || req.State == "" || len(req.State) > 256 {
		keyPouchRespondError(c, service.KeyPouchBadRequest)
		return
	}
	req.Email = email
	res, err := h.complete(c.Request.Context(), c.Param("job_id"), req)
	if err != nil {
		keyPouchRespondError(c, err)
		return
	}
	c.JSON(200, res)
}

func (h *KeyPouchImportHandler) target(ctx context.Context, job *service.KeyPouchImportJob) (int64, error) {
	group, err := h.store.TargetGroup(ctx)
	if err != nil {
		return 0, err
	}
	if job.GroupID != 0 && job.GroupID != group {
		return 0, service.KeyPouchMembershipChanged
	}
	return group, nil
}

func (h *KeyPouchImportHandler) member(ctx context.Context, job *service.KeyPouchImportJob, account *service.KeyPouchIdentity, group int64) (keyPouchResult, error) {
	completed := job.Phase == "member"
	if account == nil || (completed && (job.AccountID != account.AccountID || !account.Member)) {
		return keyPouchResult{}, service.KeyPouchMembershipChanged
	}
	if !account.Member {
		if err := h.admin.ValidateAccountGroupBindings(ctx, []int64{group}); err != nil {
			return keyPouchResult{}, err
		}
		if err := h.admin.CheckMixedChannelRisk(ctx, account.AccountID, service.PlatformOpenAI, []int64{group}); err != nil {
			return keyPouchResult{}, err
		}
	}
	if err := h.store.ConfirmMembership(ctx, account.AccountID, group, !completed && !account.Member); err != nil {
		return keyPouchResult{}, err
	}
	if !completed {
		*job = service.KeyPouchImportJob{ID: job.ID, Email: job.Email, Phase: "member", GroupID: group, AccountID: account.AccountID, ExpiresAt: time.Now().UTC()}
	}
	return keyPouchResult{Status: "member", AccountID: account.AccountID, GroupID: group}, nil
}

func (h *KeyPouchImportHandler) unseal(job *service.KeyPouchImportJob, app openai.KeyPouchRegistration) (keyPouchSession, error) {
	plain, err := h.cipher.Decrypt(job.SealedSession)
	var session keyPouchSession
	if err != nil || json.Unmarshal([]byte(plain), &session) != nil || session.State == "" || session.Nonce == "" || session.Verifier == "" {
		return session, service.KeyPouchUnavailable
	}
	if session.App != app {
		return session, service.KeyPouchOAuthMissing
	}
	return session, nil
}

func (h *KeyPouchImportHandler) start(ctx context.Context, id, email string) (res keyPouchResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = h.store.WithJob(ctx, id, email, func(ctx context.Context, job *service.KeyPouchImportJob) error {
		if job.Phase == "exchanging" && time.Now().Before(job.ExpiresAt) {
			return service.KeyPouchExchanging
		}
		group, err := h.target(ctx, job)
		if err != nil {
			return err
		}
		account, err := h.store.FindIdentity(ctx, email, "", "", group)
		if err != nil {
			return err
		}
		if account != nil || job.Phase == "member" {
			res, err = h.member(ctx, job, account, group)
			return err
		}
		app, err := h.registration(ctx)
		if err != nil {
			return err
		}
		var session keyPouchSession
		if job.Phase == "pending" && time.Now().Before(job.ExpiresAt) {
			session, err = h.unseal(job, app)
			if err != nil {
				return err
			}
		} else {
			session.App = app
			if session.State, err = openai.GenerateState(); err != nil {
				return err
			}
			if session.Verifier, err = openai.GenerateCodeVerifier(); err != nil {
				return err
			}
			if session.Nonce, err = openai.GenerateState(); err != nil {
				return err
			}
			b, err := json.Marshal(session)
			if err != nil {
				return err
			}
			sealed, err := h.cipher.Encrypt(string(b))
			if err != nil {
				return err
			}
			*job = service.KeyPouchImportJob{ID: id, Email: email, Phase: "pending", GroupID: group, SealedSession: sealed, ExpiresAt: time.Now().Add(openai.SessionTTL)}
		}
		res = keyPouchResult{Status: "authorization_required", URL: app.AuthorizationURL(session.State, session.Verifier, session.Nonce)}
		return nil
	})
	return res, err
}

func (h *KeyPouchImportHandler) complete(ctx context.Context, id string, req keyPouchCallback) (res keyPouchResult, err error) {
	var session keyPouchSession
	var attempt string
	claim, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = h.store.WithJob(claim, id, req.Email, func(ctx context.Context, job *service.KeyPouchImportJob) error {
		switch job.Phase {
		case "":
			return service.KeyPouchUnknownJob
		case "failed":
			return service.KeyPouchError(job.Failure)
		case "member":
			group, err := h.target(ctx, job)
			if err != nil {
				return err
			}
			account, err := h.store.FindIdentity(ctx, req.Email, "", "", group)
			if err != nil {
				return err
			}
			res, err = h.member(ctx, job, account, group)
			return err
		}
		if !time.Now().Before(job.ExpiresAt) {
			return service.KeyPouchExpired
		}
		if job.Phase == "exchanging" {
			return service.KeyPouchExchanging
		}
		app, err := h.registration(ctx)
		if err != nil {
			return err
		}
		session, err = h.unseal(job, app)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(req.State), []byte(session.State)) != 1 {
			return service.KeyPouchStateMismatch
		}
		attempt, err = openai.GenerateSessionID()
		if err != nil {
			return err
		}
		job.Phase, job.Attempt, job.SealedSession, job.ExpiresAt = "exchanging", attempt, "", time.Now().Add(keyPouchExchangeLease)
		return nil
	})
	cancel()
	if err != nil || res.Status == "member" {
		return res, err
	}
	// The code has been claimed durably. Finish even if the caller disconnects.
	work, done := context.WithTimeout(context.WithoutCancel(ctx), 80*time.Second)
	defer done()
	info, err := h.verifiedExchange(work, session, req)
	if err == nil {
		err = h.store.WithJob(work, id, req.Email, func(ctx context.Context, job *service.KeyPouchImportJob) error {
			if job.Phase != "exchanging" || job.Attempt != attempt || !time.Now().Before(job.ExpiresAt) {
				return service.KeyPouchExpired
			}
			group, e := h.target(ctx, job)
			if e != nil {
				return e
			}
			account, e := h.store.FindIdentity(ctx, req.Email, info.ChatGPTAccountID, info.ChatGPTUserID, group)
			if e != nil {
				return e
			}
			if account == nil {
				creds := h.oauth.BuildAccountCredentials(info)
				creds["oauth_client_registration"] = openai.KeyPouchClientRegistration
				creds["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
				name := req.Email
				if len(name) > 100 {
					name = "OpenAI " + info.ChatGPTAccountID
					if len(name) > 100 {
						name = "OpenAI OAuth"
					}
				}
				created, e := h.admin.CreateAccount(ctx, &service.CreateAccountInput{Name: name, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: creds, GroupIDs: []int64{group}})
				if e != nil {
					return e
				}
				account, e = h.store.FindIdentity(ctx, req.Email, info.ChatGPTAccountID, info.ChatGPTUserID, group)
				if e != nil {
					return e
				}
				if account == nil || account.AccountID != created.ID || !account.Member {
					return service.KeyPouchMembershipChanged
				}
			}
			res, e = h.member(ctx, job, account, group)
			return e
		})
	}
	if err != nil {
		// A stale attempt cannot change the record of a later consent or undo a
		// committed membership after an uncertain database acknowledgement.
		var safe service.KeyPouchError
		if !errors.As(err, &safe) {
			safe = service.KeyPouchUnavailable
		}
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if e := h.store.WithJob(record, id, req.Email, func(_ context.Context, job *service.KeyPouchImportJob) error {
			if job.Phase == "exchanging" && job.Attempt == attempt {
				job.Phase, job.Attempt, job.Failure = "failed", "", string(safe)
			}
			return nil
		}); e != nil {
			return keyPouchResult{}, service.KeyPouchUnavailable
		}
		return keyPouchResult{}, safe
	}
	return res, nil
}

func (h *KeyPouchImportHandler) verifiedExchange(ctx context.Context, session keyPouchSession, req keyPouchCallback) (*service.OpenAITokenInfo, error) {
	token, err := h.exchange(ctx, session.App, req.Code, session.Verifier)
	var rejected *openai.RefreshTokenError
	if errors.As(err, &rejected) && (rejected.Code == "invalid_client" || rejected.Code == "unauthorized_client" || rejected.Code == "invalid_scope") {
		return nil, service.KeyPouchOAuthMissing
	}
	if err != nil || token == nil || token.AccessToken == "" || token.RefreshToken == "" || token.IDToken == "" || (token.Scope != "" && !keyPouchScopes(token.Scope)) {
		return nil, service.KeyPouchExchangeFailed
	}
	cfg := config.OIDCConnectConfig{IssuerURL: openai.RegisteredIssuer, ClientID: session.App.ClientID, JWKSURL: openai.RegisteredJWKSURL, AllowedSigningAlgs: "RS256", ClockSkewSeconds: 60}
	claims, err := h.verify(ctx, cfg, token.IDToken, session.Nonce)
	if err != nil || claims == nil || claims.ExpiresAt == nil || claims.EmailVerified == nil || !*claims.EmailVerified {
		return nil, service.KeyPouchUnverified
	}
	email, err := keyPouchEmail(claims.Email)
	if err != nil || email != req.Email {
		return nil, service.KeyPouchIdentityMismatch
	}
	provider, err := openai.DecodeKeyPouchCredentialClaims(token.IDToken)
	if err != nil || provider.Auth == nil {
		return nil, service.KeyPouchUnverified
	}
	auth := provider.Auth
	workspace, user := strings.TrimSpace(auth.ChatGPTAccountID), strings.TrimSpace(auth.ChatGPTUserID)
	if user == "" {
		user = strings.TrimSpace(auth.UserID)
	}
	if workspace == "" || user == "" {
		return nil, service.KeyPouchUnverified
	}
	if access, e := openai.DecodeKeyPouchCredentialClaims(token.AccessToken); e == nil && !access.AgreesWith(email, workspace, user) {
		return nil, service.KeyPouchIdentityConflict
	}
	organization := ""
	for _, org := range auth.Organizations {
		if len(auth.Organizations) == 1 || org.IsDefault {
			if organization != "" && organization != org.ID {
				return nil, service.KeyPouchIdentityConflict
			}
			organization = org.ID
		}
	}
	if len(auth.Organizations) > 1 && organization == "" {
		return nil, service.KeyPouchIdentityConflict
	}
	info := &service.OpenAITokenInfo{AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, IDToken: token.IDToken,
		ClientID: session.App.ClientID, Email: email, ChatGPTAccountID: workspace, ChatGPTUserID: user, OrganizationID: organization,
		PlanType: auth.ChatGPTPlanType, SubscriptionExpiresAt: auth.SubscriptionExpiresAt(), ExpiresIn: int64(token.ExpiresIn)}
	if token.ExpiresIn > 0 {
		info.ExpiresAt = time.Now().Unix() + int64(token.ExpiresIn)
	}
	return info, nil
}
