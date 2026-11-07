//go:build linux

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	requestbody "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

func gatewayMain(args []string) int {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	bin := fs.String("codex", "", "official Codex binary")
	out := fs.String("out", "", "new candidate directory")
	models := fs.String("models-json", "", "fixed official model catalog")
	version := fs.String("expect-version", "", "fixed official CLI version")
	commit := fs.String("source-commit", "", "fixed official source commit")
	ingressMode := fs.String("ingress-mode", "passthrough", "candidate WS ingress: passthrough or ctx_pool")
	single := fs.String("scenario", "", "run only one scenario (diagnostics, not full acceptance)")
	worker := fs.Bool("worker", false, "internal traced worker")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := validateGatewayIngressMode(*ingressMode); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		return 2
	}
	if *bin == "" || *out == "" || *models == "" || *version == "" || *commit == "" {
		fmt.Fprintln(os.Stderr, "gateway: all reference and output arguments are required")
		return 2
	}
	if !*worker {
		if _, err := os.Stat(*out); !os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "gateway: refusing to reuse candidate directory")
			return 2
		}
		if err := os.MkdirAll(*out, 0700); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		tracer, err := exec.LookPath("strace")
		if err != nil {
			fmt.Fprintln(os.Stderr, "gateway: strace required")
			return 2
		}
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		trace := filepath.Join(*out, "gateway-connect.strace")
		command := []string{"-f", "-qq", "-s", "256", "-e", "trace=connect", "-o", trace, "--", self, "gateway"}
		command = append(command, args...)
		command = append(command, "-worker")
		cmd := exec.Command(tracer, command...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		leaks := checkTrace(trace)
		writeJSON(filepath.Join(*out, "trace-validation.json"), map[string]any{"scope": "gateway process and all CLI/tool children", "outbound_leaks": leaks, "worker_exit_success": err == nil})
		if err != nil {
			fmt.Fprintln(os.Stderr, "gateway worker:", err)
			return 1
		}
		if len(leaks) > 0 {
			fmt.Fprintln(os.Stderr, "gateway: non-loopback connections observed")
			return 3
		}
		if err := finalizeCaptureTraces(*out); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return 0
	}
	for _, name := range []string{"CODEX_CA_CERTIFICATE", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	gin.SetMode(gin.TestMode)
	nativeTLS, err := recordGatewayNativeTLS()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	writeJSON(filepath.Join(*out, "tls_clienthellos.json"), nativeTLS)
	ca, serverTLS, err := newEphemeralTLS("chatgpt.com", "auth.openai.com")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	caPath := filepath.Join(*out, "local-ca.pem")
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// Each Codex transport loads this scoped CA explicitly; verification stays
	// enabled even after native TLS initialized the system root cache above.
	if customCARecording() {
		if err := os.Setenv("CODEX_CA_CERTIFICATE", caPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	modelsJSON, err = os.ReadFile(*models)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	actualVersion, err := codexVersion(*bin)
	if err != nil || actualVersion != "codex-cli "+*version {
		fmt.Fprintf(os.Stderr, "gateway: version mismatch %q (%v)\n", actualVersion, err)
		return 2
	}
	binary, err := os.ReadFile(*bin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	buildMeta := map[string]any{"codex_version": actualVersion, "source_commit": *commit, "binary_sha256": fmt.Sprintf("%x", sha256.Sum256(binary)), "models_sha256": fmt.Sprintf("%x", sha256.Sum256(modelsJSON))}
	if err := bindSourceBuildReceipt(buildMeta, *out, *bin); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		return 2
	}
	service.SetCodexCanonicalUserAgentResolver(func() string {
		return "codex_cli_rs/" + *version + " (Ubuntu 26.4.0; x86_64) xterm-256color (codex_cli_rs; " + *version + ")"
	})
	gin.SetMode(gin.TestMode)
	scenarios := []scenario{
		{Name: "ws_initial"}, {Name: "ws_tool_loop", ToolLoop: true},
		{Name: "http_fallback_tool_loop", RejectWebSocket: true, ToolLoop: true},
		{Name: "http_unauthorized_refresh", RejectWebSocket: true, UnauthorizedUntilRefresh: true},
		{Name: "ws_unauthorized_refresh", UnauthorizedUntilRefresh: true},
		{Name: "ws_compaction", ToolLoop: true, Compaction: true},
	}
	names := []string{}
	var hellos []map[string]any
	gatewayHash := ""
	for i, sc := range scenarios {
		sc.NativeState = true
		if *single != "" && *single != sc.Name {
			continue
		}
		recs, tlsRows, provenance, err := recordGatewayScenario(*bin, sc, serverTLS, caPath, int64(100+i), *out, *ingressMode)
		writeJSONL(filepath.Join(*out, sc.Name+".jsonl"), recs)
		writeJSON(filepath.Join(*out, sc.Name+"-provenance.json"), provenance)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gateway scenario %s: %v\n", sc.Name, err)
			return 1
		}
		observedHash, _ := provenance["gateway_config_sha256"].(string)
		if gatewayHash != "" && gatewayHash != observedHash {
			fmt.Fprintln(os.Stderr, "gateway: configuration changed between scenarios")
			return 2
		}
		gatewayHash = observedHash
		hellos = mergeHellos(hellos, tlsRows)
		names = append(names, sc.Name)
		fmt.Printf("gateway scenario %s: %d real socket records\n", sc.Name, len(recs))
	}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "gateway: no scenarios executed")
		return 2
	}
	writeJSON(filepath.Join(*out, "tls_clienthellos_scenarios.json"), hellos)
	meta := map[string]any{
		"codex_version": actualVersion, "source_commit": *commit, "binary_sha256": fmt.Sprintf("%x", sha256.Sum256(binary)),
		"models_sha256": fmt.Sprintf("%x", sha256.Sum256(modelsJSON)), "recorded_at": time.Now().UTC().Format(time.RFC3339),
		"recorder": "backend/tools/codexgolden gateway", "tls_mode": recorderTLSMode(), "model": "gpt-6-astra", "reasoning_effort": "xhigh", "scenarios": names, "native_tls_pending": false, "candidate_ingress_mode": *ingressMode, "gateway_config_sha256": gatewayHash,
	}
	if hash, ok := buildMeta["source_build_receipt_sha256"]; ok {
		meta["source_build_receipt_sha256"] = hash
	}
	if err := addCaptureContract(meta); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	writeJSON(filepath.Join(*out, "meta.json"), meta)
	return 0
}

func recordGatewayScenario(bin string, sc scenario, serverTLS *tls.Config, caPath string, id int64, out string, ingressMode string) ([]record, []map[string]any, map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var mu sync.Mutex
	routes := map[string]int{}
	var ingressObservations []gatewayIngressObservation
	ingressSequence := 0
	var routeErrors []string
	recordError := func(err error) { mu.Lock(); routeErrors = append(routeErrors, err.Error()); mu.Unlock() }
	var ingress string
	localTransport := &http.Transport{Proxy: nil, DisableCompression: true}
	defer localTransport.CloseIdleConnections()
	hellos := newHelloSet()
	up, err := newFakeUpstream(sc, upstreamOptions{TLS: serverTLS, ModelsJSON: modelsJSON, ModelsBarrier: true, OnHello: func(h *clientHello) {
		if h.ServerName != "localhost" {
			hellos.add(h)
		}
	}, RouteRequest: func(c net.Conn, br *bufio.Reader, method, target, proto string, headers []headerKV) (bool, bool) {
		return routeGatewayRequest(ingress, localTransport, recordError)(c, br, method, target, proto, headers)
	}})
	if err != nil {
		return nil, nil, nil, err
	}
	closed := false
	defer func() {
		if !closed {
			up.close()
		}
	}()
	origin := fmt.Sprintf("https://localhost:%d", up.port())
	up.opts.WorkspaceOrigin = origin
	sb, err := newSandbox(origin)
	if err != nil {
		return nil, nil, nil, err
	}
	defer sb.cleanup()
	if sc.Compaction {
		f, err := os.OpenFile(filepath.Join(sb.codexHome, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return nil, nil, nil, err
		}
		_, err = f.WriteString("model_auto_compact_token_limit = 1\n")
		closeErr := f.Close()
		if err != nil {
			return nil, nil, nil, err
		}
		if closeErr != nil {
			return nil, nil, nil, closeErr
		}
	}
	sb.extraEnv = []string{"CODEX_REFRESH_TOKEN_URL_OVERRIDE=" + origin + "/oauth/token"}
	if customCARecording() {
		sb.extraEnv = append(sb.extraEnv, "CODEX_CA_CERTIFICATE="+caPath)
	}
	proxy := newGatewayConnectProxy(fmt.Sprintf("127.0.0.1:%d", up.port()))
	defer proxy.close()
	proxyURL, _ := url.Parse(proxy.server.URL)
	port, _ := strconv.Atoi(proxyURL.Port())
	p := &service.Proxy{ID: id, Protocol: "http", Host: "127.0.0.1", Port: port, Status: service.StatusActive}
	account := &service.Account{
		ID: id, Name: "synthetic-codexgolden", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 2,
		ProxyID: &p.ID, Proxy: p,
		Credentials: map[string]any{"access_token": fakeJWT(map[string]any{"exp": 4102444800, "https://api.openai.com/auth": goldenAuthClaims()}), "refresh_token": "rt-golden", "chatgpt_account_id": "acct-golden", "chatgpt_user_id": "user-golden", "expires_at": float64(4102444800)},
		Extra:       gatewaySyntheticModelPolicyExtra(map[string]any{"codex_client_family": "exec", "codex_workspace_cwd": sb.cwd, "codex_sandbox_mode": "read-only", "openai_oauth_responses_websockets_v2_mode": "passthrough", "openai_oauth_responses_websockets_v2_enabled": true, "openai_passthrough": true}),
	}
	accounts := &gatewayAccounts{account: account}
	cache := &gatewayTokenCache{}
	oauth := service.NewOpenAIOAuthService(&gatewayProxies{proxy: p}, repository.NewOpenAIOAuthClient())
	tokens := service.NewOpenAITokenProvider(accounts, cache, oauth)
	tokens.SetRefreshAPI(service.NewOAuthRefreshAPI(accounts, cache), service.NewOpenAITokenRefresher(oauth, accounts))
	cfg := gatewayScenarioConfig()
	if err := configureGatewayIngressMode(cfg, account, ingressMode); err != nil {
		return nil, nil, nil, err
	}
	configHash := gatewayConfigFingerprint(cfg)
	upstream := gatewayObservedHTTPUpstream{HTTPUpstream: repository.NewHTTPUpstream(cfg)}
	svc := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, tokens, nil, nil, nil, nil, nil, nil)
	svc.StopOpenAICodexTicketHarvester()
	defer svc.CloseOpenAIWSPool()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		c.Set("api_key", &service.APIKey{ID: 1, UserID: 1, User: &service.User{ID: 1}})
		acc, err := accounts.GetByID(r.Context(), id)
		if err != nil {
			recordError(err)
			http.Error(w, "account unavailable", 500)
			return
		}
		kind := r.Method + " " + r.URL.Path
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			kind = "WS " + r.URL.Path
		}
		mu.Lock()
		routes[kind]++
		mu.Unlock()
		if r.URL.Path == "/backend-api/codex/models" {
			modelsCtx := service.WithCodexModelsClientContext(r.Context(), r.Header.Get("User-Agent"), r.Header.Get("originator"))
			result, err := svc.FetchCodexModelsManifest(modelsCtx, acc, r.URL.Query().Get("client_version"), r.Header.Get("If-None-Match"))
			if err != nil {
				recordError(err)
				http.Error(w, "models failed", 502)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if result.ETag != "" {
				w.Header().Set("ETag", result.ETag)
			}
			if result.NotModified {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(result.Body)
			return
		}
		if strings.HasPrefix(kind, "WS ") {
			var httpObservation *gatewayIngressHTTPObservation
			r, httpObservation = withGatewayIngressHTTPObservation(r)
			c.Request = r
			service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
			mu.Lock()
			ingressSequence++
			entrySequence := ingressSequence
			mu.Unlock()
			if err := verifyGatewayModeResolution(cfg, acc, ingressMode); err != nil {
				recordError(err)
				http.Error(w, "ingress mode mismatch", 500)
				return
			}
			before := svc.SnapshotOpenAIWSPoolMetrics().AcquireTotal
			observation := gatewayIngressObservation{Sequence: entrySequence, ActualMode: ingressMode, Upgrade426Before: up.upgrade426Responses.Load(), AccountMode: acc.ResolveOpenAIResponsesWebSocketV2Mode(cfg.Gateway.OpenAIWS.IngressModeDefault), ResolverTransport: string(service.NewOpenAIWSProtocolResolver(cfg).Resolve(acc).Transport), PoolAcquiresBefore: before}
			defer func() {
				observation.PoolAcquiresAfter = svc.SnapshotOpenAIWSPoolMetrics().AcquireTotal
				observation.Upgrade426After = up.upgrade426Responses.Load()
				observation.HTTPResponses = httpObservation.responses.Load()
				if observation.PoolAcquiresAfter == observation.PoolAcquiresBefore && observation.HTTPResponses > 0 {
					observation.ActualMode = "http_bridge"
					observation.FallbackReason = "prior_upstream_426"
				}
				mu.Lock()
				ingressObservations = append(ingressObservations, observation)
				mu.Unlock()
			}()

			conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
			if err != nil {
				recordError(err)
				return
			}
			defer conn.CloseNow()
			conn.SetReadLimit(service.ResolveOpenAIWSClientReadLimitBytes(cfg))
			_, first, err := service.ReadOpenAIWSClientMessage(r.Context(), conn, 30*time.Second, coderws.StatusPolicyViolation, "missing first response.create message")
			if err == nil {
				var token string
				token, _, err = svc.GetAccessToken(r.Context(), acc)
				if err == nil {
					err = svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, acc, token, first, nil)
				}
			}
			if err != nil {
				var closeErr *service.OpenAIWSClientCloseError
				if errors.As(err, &closeErr) {
					_ = conn.Close(closeErr.StatusCode(), closeErr.Reason())
				} else {
					_ = conn.Close(coderws.StatusInternalError, "gateway forwarding failed")
				}
				// Normal client shutdown and scripted upstream 426/401 are
				// validated through the real captures and CLI exit below.
			}
			return
		}
		body, err := requestbody.ReadRequestBodyWithPrealloc(r)
		if err == nil {
			_, err = svc.Forward(r.Context(), c, acc, body)
		}
		if err != nil {
			recordError(err)
			if !c.Writer.Written() {
				http.Error(w, "forwarding failed", 502)
			}
		}
	}))
	defer server.Close()
	ingress = strings.TrimPrefix(server.URL, "http://")
	_, runErr := runCodex(ctx, bin, "", sb, "Reply with ok.")
	if err := os.CopyFS(filepath.Join(out, "driver", sc.Name), os.DirFS(sb.root)); err != nil {
		return nil, nil, nil, fmt.Errorf("save synthetic driver evidence: %w", err)
	}
	svc.CloseOpenAIWSPool()
	localTransport.CloseIdleConnections()
	proxy.close()
	recs := up.close()
	closed = true
	if err := os.MkdirAll(filepath.Join(out, "raw"), 0700); err != nil {
		return nil, nil, nil, err
	}
	writeJSONL(filepath.Join(out, "raw", sc.Name+".jsonl"), recs)
	writeJSON(filepath.Join(out, "raw", sc.Name+"-tls.json"), hellos.export())
	writeJSON(filepath.Join(out, "raw", sc.Name+"-tls-connections.json"), up.tlsConnections())
	writeJSON(filepath.Join(out, "raw", sc.Name+"-h2-connection-ends.json"), up.h2ConnectionEnds())
	if err := up.validateModelsBarrier(); err != nil {
		return nil, nil, nil, err
	}
	stderr, readErr := os.ReadFile(filepath.Join(sb.root, "codex.stderr"))
	if readErr != nil {
		return recs, hellos.export(), nil, readErr
	}
	if err := os.WriteFile(filepath.Join(out, sc.Name+"-cli.stderr"), stderr, 0600); err != nil {
		return recs, hellos.export(), nil, err
	}
	mu.Lock()
	provenance := map[string]any{"scenario": sc.Name, "candidate_ingress_mode": ingressMode, "gateway_config_sha256": configHash, "ingress_observations": ingressObservations, "upstream_upgrade_426_responses": up.upgrade426Responses.Load(), "service_routes": routes, "route_errors": routeErrors, "cli_exit_success": runErr == nil, "context_error": fmt.Sprint(ctx.Err()), "synthetic_account_id": id, "model_policy_source": "explicit_synthetic_astra_health_snapshot", "model_policy_bypassed": false, "credential_writes": accounts.credentialWrites, "proxy_targets": proxy.targets, "proxy_denied": proxy.denied}
	mu.Unlock()
	if err := validateGatewayModeEvidence(captureMetadata{GatewayMode: ingressMode, GatewayConfigHash: configHash}, gatewayModeEvidence{Scenario: sc.Name, Upgrade426Responses: up.upgrade426Responses.Load(), Mode: ingressMode, ConfigHash: configHash, Observations: ingressObservations}, routes["WS /backend-api/codex/responses"]); err != nil {
		return recs, hellos.export(), provenance, err
	}
	if err := validateGatewayModeWireEvidence(gatewayModeEvidence{Scenario: sc.Name, Upgrade426Responses: up.upgrade426Responses.Load(), Mode: ingressMode, ConfigHash: configHash, Observations: ingressObservations}, recs); err != nil {
		return recs, hellos.export(), provenance, err
	}
	normalized, err := normalizeRecords(recs, sb, up.port())
	if err != nil {
		return nil, nil, provenance, err
	}
	// The allowlisted CONNECT proxy maps these names to this exact local
	// recorder. Canonicalize only the transport endpoint fields, preserving
	// raw captures and every user/body string. Reference CLI uses localhost.
	for i := range normalized {
		if normalized[i].Hop != "gateway_upstream" {
			continue
		}
		for j, h := range normalized[i].Headers {
			if (strings.EqualFold(h.Name, "host") || h.Name == ":authority") && (h.Value == "chatgpt.com" || h.Value == "auth.openai.com") {
				normalized[i].Headers[j].Value = "<UPSTREAM>"
			}
		}
	}
	normalizedHellos := up.coreHellos()
	for _, hello := range normalizedHellos {
		if host, _ := hello["server_name"].(string); host == "chatgpt.com" || host == "auth.openai.com" {
			hello["server_name"] = "localhost"
		}
	}
	provenance["fixture_endpoint_mapping"] = map[string]string{"chatgpt.com": "localhost", "auth.openai.com": "localhost"}
	if runErr != nil || ctx.Err() != nil {
		return normalized, hellos.export(), provenance, fmt.Errorf("CLI failed: exec=%v context=%v: %s", runErr, ctx.Err(), tail(stderr, 2000))
	}
	if err := validateScenario(sc, recs); err != nil {
		return normalized, hellos.export(), provenance, err
	}
	if len(routeErrors) > 0 || len(proxy.denied) > 0 {
		return normalized, hellos.export(), provenance, errors.New("local gateway routing failed")
	}
	for _, rec := range recs {
		core := rec.Kind == "ws_message" || strings.Contains(rec.Target, "/codex/responses") || strings.Contains(rec.Target, "/codex/models")
		if core && rec.Hop != "gateway_upstream" {
			return normalized, hellos.export(), provenance, errors.New("core request bypassed gateway")
		}
	}
	if routes["WS /backend-api/codex/responses"] == 0 || routes["GET /backend-api/codex/models"] == 0 {
		return normalized, hellos.export(), provenance, errors.New("missing gateway entry path")
	}
	if sc.UnauthorizedUntilRefresh && accounts.credentialWrites == 0 {
		return normalized, hellos.export(), provenance, errors.New("gateway did not persist refreshed credentials")
	}
	return normalized, normalizedHellos, provenance, nil
}

func gatewayScenarioConfig() *config.Config {
	cfg := &config.Config{}
	// These fixtures isolate the CLI request lifecycle. Automatic Tibo health
	// probes require independent coverage outside this wire projection.
	disableTibo := false
	cfg.Gateway.OpenAITiboRoute.Enabled = &disableTibo
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = "passthrough"
	cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
	// Preserve the production default: OAuth resolves custom CA from the actual
	// environment; a forced H2 switch would contaminate the native reference.
	cfg.Gateway.OpenAIWS.CodexHTTPH2Enabled = false
	cfg.Gateway.OpenAIWS.SessionDisableEnabled = true
	return cfg
}

// This is an explicit synthetic scheduling snapshot, never a live capability
// claim. Production model policy still validates its model, source and timestamp.
func gatewaySyntheticModelPolicyExtra(extra map[string]any) map[string]any {
	out := make(map[string]any, len(extra)+1)
	for key, value := range extra {
		out[key] = value
	}
	out[service.OpenAIAstraPolicyExtraKey] = map[string]any{
		"model":          "gpt-6-astra",
		"recent_samples": []any{map[string]any{"verdict": "healthy", "at": "2020-01-01T00:00:00Z"}},
	}
	return out
}
