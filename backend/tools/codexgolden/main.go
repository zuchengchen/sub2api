//go:build linux

// Command codexgolden records the wire behavior of a locally installed Codex
// CLI against local fake endpoints and writes golden fixtures that sub2api's
// outbound stack is tested against.
//
// It never contacts OpenAI: Codex runs with an isolated HOME/CODEX_HOME, a
// synthetic ChatGPT auth.json (fake JWTs, far-future expiry) and every base
// URL pointed at 127.0.0.1. When strace is available every connect(2) is
// traced and any non-loopback destination fails the run.
//
// Usage (from backend/):
//
//	go run ./tools/codexgolden -codex "$(command -v codex)" \
//	    -out internal/pkg/codexwire/testdata/golden/0.163.0
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "compare" {
		os.Exit(compareMain(os.Args[2:]))
	}
	codexBin := flag.String("codex", "codex", "path to the Codex CLI binary to record")
	outDir := flag.String("out", "", "output directory for golden files (required)")
	timeout := flag.Duration("timeout", 90*time.Second, "per-run timeout")
	useStrace := flag.Bool("strace", true, "trace connect(2) and fail on non-loopback destinations")
	modelsPath := flag.String("models-json", "", "models.json served for GET /codex/models (codex-rs/models-manager/models.json at the recorded tag)")
	expectVersion := flag.String("expect-version", "", "require this exact CLI version before recording")
	sourceCommit := flag.String("source-commit", "", "official commit resolved from the recorded release tag")
	flag.StringVar(&keepDir, "keep", "", "copy each sandbox (trace, stderr) here for debugging")
	traceDNS := flag.Bool("trace-dns", false, "also trace sendto/sendmsg to see DNS query names (debug)")
	flag.Parse()
	if *traceDNS {
		traceSyscalls = "trace=connect,sendto,sendmsg"
	}
	if *modelsPath != "" {
		data, err := os.ReadFile(*modelsPath)
		if err != nil {
			fatal(err)
		}
		modelsJSON = data
	}
	if *outDir == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}
	version, err := codexVersion(*codexBin)
	if err != nil {
		fatal(err)
	}
	if *expectVersion != "" && version != "codex-cli "+*expectVersion {
		fatal(fmt.Errorf("version %q does not match requested %q", version, *expectVersion))
	}
	fmt.Printf("recording %s\n", version)

	tracer := ""
	if *useStrace {
		if p, err := exec.LookPath("strace"); err == nil {
			tracer = p
		} else {
			fatal(errors.New("strace is required unless explicitly disabled with -strace=false"))
		}
	}

	meta := map[string]any{
		"codex_version": version,
		"recorded_at":   time.Now().UTC().Format(time.RFC3339),
		"recorder":      "backend/tools/codexgolden",
	}
	if *sourceCommit != "" {
		meta["source_commit"] = *sourceCommit
	}
	if len(modelsJSON) > 0 {
		meta["models_sha256"] = fmt.Sprintf("%x", sha256.Sum256(modelsJSON))
	}
	binPath, err := exec.LookPath(*codexBin)
	if err != nil {
		fatal(err)
	}
	bin, err := os.Open(binPath)
	if err != nil {
		fatal(err)
	}
	hash := sha256.New()
	_, hashErr := io.Copy(hash, bin)
	_ = bin.Close()
	if hashErr != nil {
		fatal(hashErr)
	}
	meta["binary_sha256"] = fmt.Sprintf("%x", hash.Sum(nil))
	var leaks []string

	hellos, tlsLeaks, err := recordTLS(*codexBin, tracer, *timeout)
	if err != nil {
		fatal(fmt.Errorf("tls run: %w", err))
	}
	leaks = append(leaks, tlsLeaks...)
	writeJSON(filepath.Join(*outDir, "tls_clienthellos.json"), hellos)
	meta["tls_lanes"] = len(hellos)

	scenarios := []scenario{
		{Name: "ws_initial"},
		{Name: "ws_tool_loop", ToolLoop: true},
		{Name: "http_fallback_tool_loop", RejectWebSocket: true, ToolLoop: true},
		{Name: "http_unauthorized_refresh", RejectWebSocket: true, UnauthorizedUntilRefresh: true},
		{Name: "ws_unauthorized_refresh", UnauthorizedUntilRefresh: true},
		{Name: "ws_compaction", ToolLoop: true, Compaction: true},
	}
	var names []string
	var customCAHellos []map[string]any
	for _, sc := range scenarios {
		recs, scHellos, scLeaks, err := recordScenario(*codexBin, tracer, sc, *timeout)
		if err != nil {
			fatal(fmt.Errorf("scenario %s: %w", sc.Name, err))
		}
		leaks = append(leaks, scLeaks...)
		writeJSONL(filepath.Join(*outDir, sc.Name+".jsonl"), recs)
		names = append(names, sc.Name)
		customCAHellos = mergeHellos(customCAHellos, scHellos)
		fmt.Printf("scenario %s: %d records\n", sc.Name, len(recs))
	}
	// With CODEX_CA_CERTIFICATE set Codex switches every lane to rustls; kept
	// separately because production users normally have no custom CA.
	writeJSON(filepath.Join(*outDir, "tls_clienthellos_custom_ca.json"), customCAHellos)
	meta["scenarios"] = names
	meta["strace_guard"] = tracer != ""
	meta["outbound_leaks"] = leaks
	// Requests Codex tried to send to hard-coded hosts; refused locally.
	meta["blocked_outbound_requests"] = blackholeTargets()
	writeJSON(filepath.Join(*outDir, "meta.json"), meta)
	if len(leaks) > 0 {
		fmt.Fprintf(os.Stderr, "ERROR: non-loopback connections observed:\n  %s\n", strings.Join(leaks, "\n  "))
		os.Exit(3)
	}
	fmt.Println("done")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "codexgolden:", err)
	os.Exit(1)
}

func codexVersion(bin string) (string, error) {
	sb, err := newSandbox("http://127.0.0.1:1")
	if err != nil {
		return "", err
	}
	defer sb.cleanup()
	cmd := exec.Command(bin, "--version")
	cmd.Dir = sb.cwd
	cmd.Env = []string{"HOME=" + sb.home, "CODEX_HOME=" + sb.codexHome, "PATH=" + os.Getenv("PATH")}
	cmd.Env = append(cmd.Env, blackholeProxyEnv()...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", bin, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// modelsJSON is served for GET /codex/models when set via -models-json.
var modelsJSON []byte

// keepDir, when set via -keep, receives a copy of each sandbox (strace output,
// codex stderr) for debugging. Never commit it.
var keepDir string

// traceSyscalls is the strace filter; -trace-dns adds sendto/sendmsg so DNS
// query names show up in the kept trace.
var traceSyscalls = "trace=connect"

// sandbox is an isolated HOME/CODEX_HOME/cwd for one Codex run.
type sandbox struct {
	root, home, codexHome, cwd string
	extraEnv                   []string
}

// newSandbox writes config.toml and a synthetic ChatGPT auth.json. baseURL is
// both the Responses provider and the ChatGPT bootstrap origin.
func newSandbox(baseURL string) (*sandbox, error) {
	root, err := os.MkdirTemp("", "codexgolden-")
	if err != nil {
		return nil, err
	}
	sb := &sandbox{root: root, home: filepath.Join(root, "home"), cwd: filepath.Join(root, "work")}
	sb.codexHome = filepath.Join(sb.home, ".codex")
	for _, d := range []string{sb.codexHome, sb.cwd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	config := fmt.Sprintf(`model = "gpt-5.5"
openai_base_url = "%[1]s/backend-api/codex"
chatgpt_base_url = "%[1]s/backend-api/"
check_for_update_on_startup = false
cli_auth_credentials_store = "file"
`, baseURL)
	if err := os.WriteFile(filepath.Join(sb.codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		return nil, err
	}
	auth, _ := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      fakeJWT(map[string]any{"email": "golden@example.com", "exp": 4102444800, "https://api.openai.com/auth": goldenAuthClaims()}),
			"access_token":  fakeJWT(map[string]any{"exp": 4102444800, "https://api.openai.com/auth": goldenAuthClaims()}),
			"refresh_token": "rt-golden",
			"account_id":    "acct-golden",
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(filepath.Join(sb.codexHome, "auth.json"), auth, 0o600); err != nil {
		return nil, err
	}
	return sb, nil
}

func (sb *sandbox) cleanup() { _ = os.RemoveAll(sb.root) }

func goldenAuthClaims() map[string]any {
	return map[string]any{
		"chatgpt_plan_type":  "plus",
		"chatgpt_user_id":    "user-golden",
		"chatgpt_account_id": "acct-golden",
	}
}

func fakeJWT(claims map[string]any) string {
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body, _ := json.Marshal(claims)
	return head + "." + enc.EncodeToString(body) + "." + enc.EncodeToString([]byte("golden-signature"))
}

// runCodex runs `codex exec` in the sandbox until it exits or ctx is done.
func runCodex(ctx context.Context, codexBin, tracer string, sb *sandbox, prompt string) (traceFile string, err error) {
	args := []string{"exec", "--skip-git-repo-check", "-C", sb.cwd, prompt}
	name := codexBin
	if tracer != "" {
		traceFile = filepath.Join(sb.root, "connect.strace")
		args = append([]string{"-f", "-qq", "-s", "256", "-e", traceSyscalls, "-o", traceFile, "--", codexBin}, args...)
		name = tracer
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = sb.cwd
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	if logf, ferr := os.Create(filepath.Join(sb.root, "codex.stderr")); ferr == nil {
		cmd.Stderr = logf
		defer func() { _ = logf.Close() }()
	}
	cmd.Env = append([]string{
		"HOME=" + sb.home,
		"CODEX_HOME=" + sb.codexHome,
		"PATH=" + os.Getenv("PATH"),
		"TERM=xterm-256color",
		"SHELL=/bin/bash",
		"LANG=C.UTF-8",
		// No desktop keyring / Secret Service access.
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/nonexistent/codexgolden-dbus",
	}, sb.extraEnv...)
	cmd.Env = append(cmd.Env, blackholeProxyEnv()...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
		err = nil // stopped on purpose
	}
	return traceFile, err
}

// blackhole is a local forward proxy that refuses every request and records
// the target. Codex has hard-coded chatgpt.com call sites that ignore the
// configured base URLs; routing non-loopback traffic here keeps the run
// offline while localhost traffic bypasses it via NO_PROXY.
var blackhole struct {
	once    sync.Once
	port    int
	mu      sync.Mutex
	targets map[string]int
}

func startBlackholeProxy() {
	blackhole.once.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fatal(err)
		}
		blackhole.port = tcpPort(ln.Addr())
		blackhole.targets = map[string]int{}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer func() { _ = c.Close() }()
					_ = c.SetDeadline(time.Now().Add(5 * time.Second))
					line, err := bufio.NewReader(c).ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						blackhole.mu.Lock()
						blackhole.targets[fields[0]+" "+fields[1]]++
						blackhole.mu.Unlock()
					}
					_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\ncontent-length: 0\r\nconnection: close\r\n\r\n")
				}(c)
			}
		}()
	})
}

func blackholeProxyEnv() []string {
	startBlackholeProxy()
	proxy := fmt.Sprintf("http://127.0.0.1:%d", blackhole.port)
	noProxy := "localhost,127.0.0.1,::1"
	return []string{
		"HTTPS_PROXY=" + proxy, "https_proxy=" + proxy,
		"HTTP_PROXY=" + proxy, "http_proxy=" + proxy,
		"ALL_PROXY=" + proxy, "all_proxy=" + proxy,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
	}
}

func blackholeTargets() map[string]int {
	blackhole.mu.Lock()
	defer blackhole.mu.Unlock()
	out := make(map[string]int, len(blackhole.targets))
	for k, v := range blackhole.targets {
		out[k] = v
	}
	return out
}

// helloSet dedupes ClientHellos by fingerprint and counts connections.
type helloSet struct {
	mu     sync.Mutex
	unique map[string]*seenHello
	lanes  map[string]bool
}

type seenHello struct {
	hello *clientHello
	count int
}

func newHelloSet() *helloSet {
	return &helloSet{unique: map[string]*seenHello{}, lanes: map[string]bool{}}
}

func (s *helloSet) add(ch *clientHello) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ch.fingerprintKey()
	if h, ok := s.unique[key]; ok {
		h.count++
	} else {
		s.unique[key] = &seenHello{hello: ch, count: 1}
	}
	s.lanes[ch.Lane] = true
}

func (s *helloSet) hasLanes(lanes ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range lanes {
		if !s.lanes[l] {
			return false
		}
	}
	return true
}

func (s *helloSet) export() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.unique))
	for k := range s.unique {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		h := s.unique[k]
		raw, _ := json.Marshal(h.hello)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["observed_connections"] = h.count
		m["cipher_suites_hex"] = hexList(h.hello.CipherSuites)
		m["extensions_hex"] = hexList(h.hello.Extensions)
		m["signature_algorithms_hex"] = hexList(h.hello.SignatureSchemes)
		m["supported_groups_hex"] = hexList(h.hello.SupportedGroups)
		out = append(out, m)
	}
	return out
}

func mergeHellos(dst, src []map[string]any) []map[string]any {
	seen := map[string]bool{}
	for _, m := range dst {
		seen[fmt.Sprint(m["raw_hex"])] = true
	}
	for _, m := range src {
		if !seen[fmt.Sprint(m["raw_hex"])] {
			dst = append(dst, m)
		}
	}
	return dst
}

// recordScenario terminates TLS with an ephemeral CA (CODEX_CA_CERTIFICATE)
// and records every request. Workspace routing requires an https origin, so
// the fake backend must speak TLS.
func recordScenario(codexBin, tracer string, sc scenario, timeout time.Duration) ([]record, []map[string]any, []string, error) {
	caPEM, serverTLS, err := newEphemeralTLS()
	if err != nil {
		return nil, nil, nil, err
	}
	hellos := newHelloSet()
	up, err := newFakeUpstream(sc, upstreamOptions{TLS: serverTLS, ModelsJSON: modelsJSON, OnHello: hellos.add})
	if err != nil {
		return nil, nil, nil, err
	}
	origin := fmt.Sprintf("https://localhost:%d", up.port())
	up.opts.WorkspaceOrigin = origin
	sb, err := newSandbox(origin)
	if err != nil {
		return nil, nil, nil, err
	}
	defer sb.cleanup()
	caPath := filepath.Join(sb.root, "golden-ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		return nil, nil, nil, err
	}
	if sc.Compaction {
		config, err := os.OpenFile(filepath.Join(sb.codexHome, "config.toml"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return nil, nil, nil, err
		}
		_, err = io.WriteString(config, "model_auto_compact_token_limit = 1\n")
		_ = config.Close()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	sb.extraEnv = []string{
		"CODEX_CA_CERTIFICATE=" + caPath,
		"CODEX_REFRESH_TOKEN_URL_OVERRIDE=" + origin + "/oauth/token",
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	traceFile, runErr := runCodex(ctx, codexBin, tracer, sb, "Reply with ok.")
	if keepDir != "" {
		_ = exec.Command("cp", "-r", sb.root, filepath.Join(keepDir, sc.Name+"-sandbox")).Run()
	}
	recs := up.close()
	if runErr != nil && len(recs) == 0 {
		return nil, nil, nil, fmt.Errorf("codex exec: %w", runErr)
	}
	if !hasResponsesTraffic(recs) {
		stderr, _ := os.ReadFile(filepath.Join(sb.root, "codex.stderr"))
		return nil, nil, nil, fmt.Errorf("no Responses traffic recorded; codex stderr tail:\n%s", tail(stderr, 2000))
	}
	if runErr != nil || ctx.Err() != nil {
		stderr, _ := os.ReadFile(filepath.Join(sb.root, "codex.stderr"))
		return nil, nil, nil, fmt.Errorf("incomplete run (exec=%v context=%v): %s", runErr, ctx.Err(), tail(stderr, 2000))
	}
	if err := validateScenario(sc, recs); err != nil {
		return nil, nil, nil, err
	}
	return normalizeRecords(recs, sb, up.port()), hellos.export(), checkTrace(traceFile), nil
}

func validateScenario(sc scenario, recs []record) error {
	var prewarm, turns, refresh int
	var tool, trigger, compact bool
	for _, rec := range recs {
		if strings.HasSuffix(rec.Target, "/oauth/token") {
			refresh++
		}
		if rec.Kind != "ws_message" && (rec.Method != "POST" || !strings.HasSuffix(rec.Target, "/codex/responses")) {
			continue
		}
		var body struct {
			Generate *bool `json:"generate"`
			Input    []struct {
				Type string `json:"type"`
			} `json:"input"`
		}
		if err := json.Unmarshal(rec.BodyJSON, &body); err != nil {
			return fmt.Errorf("invalid Responses body: %w", err)
		}
		if body.Generate != nil && !*body.Generate {
			prewarm++
		} else {
			turns++
		}
		if sc.RejectWebSocket && rec.Kind == "ws_message" {
			return errors.New("HTTP fallback scenario unexpectedly sent WebSocket messages")
		}
		for _, item := range body.Input {
			switch item.Type {
			case "function_call_output":
				tool = true
			case "compaction_trigger":
				trigger = true
			case "compaction":
				compact = true
			}
		}
	}
	if turns == 0 || (!sc.RejectWebSocket && prewarm != 1) || (sc.ToolLoop && !tool) || (sc.UnauthorizedUntilRefresh && refresh != 1) || (sc.Compaction && (!trigger || !compact)) {
		return fmt.Errorf("incomplete scenario: turns=%d prewarm=%d refresh=%d tool=%v compaction_trigger=%v compaction_result=%v", turns, prewarm, refresh, tool, trigger, compact)
	}
	return nil
}

func hasResponsesTraffic(recs []record) bool {
	for _, r := range recs {
		if r.Kind == "ws_message" || (r.Kind == "http_request" && strings.Contains(r.Target, "/codex/responses")) {
			return true
		}
	}
	return false
}

func tail(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

// recordTLS captures the production TLS lanes (no custom CA). Bootstrap
// traffic goes to a plaintext fake that routes the workspace to a raw
// listener; that listener records each ClientHello and drops the
// connection, so the WebSocket lane (rustls) and the HTTP fallback lane
// (native-tls) both show up as Codex retries.
func recordTLS(codexBin, tracer string, timeout time.Duration) ([]map[string]any, []string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = ln.Close() }()
	hellos := newHelloSet()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				rec, err := readTLSRecord(bufio.NewReader(c))
				if err != nil {
					return
				}
				ch, err := parseClientHello(rec)
				if err != nil {
					return
				}
				hellos.add(ch)
				if hellos.hasLanes("rustls", "openssl") {
					time.AfterFunc(2*time.Second, cancel)
				}
			}(c)
		}
	}()
	tlsOrigin := fmt.Sprintf("https://localhost:%d", tcpPort(ln.Addr()))
	bootstrap, err := newFakeUpstream(scenario{Name: "tls"}, upstreamOptions{WorkspaceOrigin: tlsOrigin, ModelsJSON: modelsJSON})
	if err != nil {
		return nil, nil, err
	}
	defer bootstrap.close()
	sb, err := newSandbox(tlsOrigin)
	if err != nil {
		return nil, nil, err
	}
	defer sb.cleanup()
	// Bootstrap (accounts/check, models, plugins) goes to the plaintext fake;
	// the Responses provider is routed to the TLS capture listener.
	config := fmt.Sprintf(`model = "gpt-5.5"
openai_base_url = "%s/backend-api/codex"
chatgpt_base_url = "http://127.0.0.1:%d/backend-api/"
check_for_update_on_startup = false
cli_auth_credentials_store = "file"
`, tlsOrigin, bootstrap.port())
	if err := os.WriteFile(filepath.Join(sb.codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		return nil, nil, err
	}
	traceFile, runErr := runCodex(ctx, codexBin, tracer, sb, "Reply with ok.")
	out := hellos.export()
	if len(out) == 0 {
		stderr, _ := os.ReadFile(filepath.Join(sb.root, "codex.stderr"))
		return nil, nil, fmt.Errorf("no ClientHello captured (codex: %v); stderr tail:\n%s", runErr, tail(stderr, 2000))
	}
	return out, checkTrace(traceFile), nil
}

// newEphemeralTLS creates a throwaway CA plus a localhost leaf. ALPN is pinned
// to http/1.1 so the recording keeps hyper's h1 framing.
func newEphemeralTLS() ([]byte, *tls.Config, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "codexgolden CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	cfg := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	}
	return caPEM, cfg, nil
}

func readTLSRecord(br *bufio.Reader) ([]byte, error) {
	head := make([]byte, 5)
	if _, err := io.ReadFull(br, head); err != nil {
		return nil, err
	}
	if head[0] != 0x16 {
		return nil, errors.New("not a handshake record")
	}
	n := int(binary.BigEndian.Uint16(head[3:5]))
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return append(head, body...), nil
}

var (
	straceInet  = regexp.MustCompile(`sa_family=AF_INET, sin_port=htons\((\d+)\), sin_addr=inet_addr\("([^"]+)"\)`)
	straceInet6 = regexp.MustCompile(`sa_family=AF_INET6, sin6_port=htons\((\d+)\).*?inet_pton\(AF_INET6, "([^"]+)"`)
)

// checkTrace lists connect(2) destinations that are not loopback, plus any
// DNS traffic (which would mean Codex tried to resolve an external name).
func checkTrace(path string) []string {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return []string{"strace output unreadable: " + err.Error()}
	}
	var leaks []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		var port, host string
		if m := straceInet.FindStringSubmatch(line); m != nil {
			port, host = m[1], m[2]
		} else if m := straceInet6.FindStringSubmatch(line); m != nil {
			port, host = m[1], m[2]
		} else {
			continue
		}
		ip := net.ParseIP(host)
		p, _ := strconv.Atoi(port)
		if ip != nil && ip.IsLoopback() && p != 53 {
			continue
		}
		if ip != nil && ip.IsUnspecified() {
			continue
		}
		key := net.JoinHostPort(host, port)
		if !seen[key] {
			seen[key] = true
			leaks = append(leaks, key)
		}
	}
	return leaks
}

func normalizeRecords(recs []record, sb *sandbox, port int) []record {
	replacer := strings.NewReplacer(
		sb.cwd, "<CWD>",
		sb.codexHome, "<CODEX_HOME>",
		sb.home, "<HOME>",
		sb.root, "<TMP>",
		fmt.Sprintf("127.0.0.1:%d", port), "<UPSTREAM>",
		fmt.Sprintf("localhost:%d", port), "<UPSTREAM>",
	)
	out := make([]record, 0, len(recs))
	for _, r := range recs {
		raw, err := json.Marshal(r)
		if err != nil {
			continue
		}
		var n record
		if err := json.Unmarshal([]byte(replacer.Replace(string(raw))), &n); err != nil {
			continue
		}
		out = append(out, n)
	}
	return out
}

func writeJSON(path string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		fatal(err)
	}
}

func writeJSONL(path string, recs []record) {
	f, err := os.Create(path)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	for _, r := range recs {
		data, err := json.Marshal(r)
		if err != nil {
			fatal(err)
		}
		_, _ = w.Write(append(data, '\n'))
	}
	if err := w.Flush(); err != nil {
		fatal(err)
	}
}
