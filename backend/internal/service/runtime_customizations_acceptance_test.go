package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRuntimeCustomizationsAcceptance(t *testing.T) {
	t.Run("unified GPT scope and fragment policy", func(t *testing.T) {
		groupID := int64(17)
		scope := NewContentModerationScopeSnapshot(&groupID, "\u3000 gPt-production \t")
		require.True(t, scope.InScope)
		require.Equal(t, int64(17), *scope.GroupID)

		// Routing may change the selected account or fallback group after entry,
		// but the request-scoped decision remains the original value snapshot.
		groupID = 29
		fallback := NewContentModerationScopeSnapshot(&groupID, "Claude")
		require.False(t, fallback.InScope)
		require.True(t, scope.InScope)
		require.Equal(t, int64(17), *scope.GroupID)

		body := []byte(`{
			"system":"system policy",
			"messages":[
				{"role":"developer","content":"blocked-token"},
				{"role":"user","content":[{"type":"input_text","text":"user text"},{"type":"input_file","filename":"brief.txt","file_url":"https://files.example/brief.txt"}]},
				{"role":"assistant","content":"assistant text"},
				{"role":"tool","content":"tool output"}
			]
		}`)
		fragments := ExtractContentModerationFragments(ContentModerationProtocolOpenAIResponses, body)
		roles := make(map[string]bool)
		kinds := make(map[string]bool)
		for _, fragment := range fragments {
			roles[fragment.Role] = true
			kinds[fragment.Kind] = true
			require.Len(t, fragment.Hash, 64)
		}
		for _, role := range []string{"system", "developer", "user", "assistant", "tool"} {
			require.True(t, roles[role], "missing role %q", role)
		}
		require.True(t, kinds["file"])
		require.True(t, kinds["url"])

		cfg := defaultContentModerationConfig()
		cfg.Enabled = true
		cfg.Mode = ContentModerationModePreBlock
		cfg.AutoBanEnabled = false
		cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordOnly
		runtime := &contentModerationRuntimeSnapshot{
			riskControlEnabled: true,
			config:             cfg,
			keywordMatcher:     newContentModerationKeywordMatcher([]string{"blocked-token"}),
		}
		repo := &contentModerationTestRepo{}
		svc := &ContentModerationService{repo: repo}
		input := ContentModerationCheckInput{Body: body, Scope: &scope, Protocol: ContentModerationProtocolOpenAIResponses}
		decision := svc.checkUnifiedFragments(context.Background(), input, runtime)
		require.True(t, decision.Blocked)
		require.Equal(t, ContentModerationActionKeywordBlock, decision.Action)
		require.Len(t, repo.snapshotLogs(), 1)

		nonGPTInput := input
		nonGPTInput.Scope = &fallback
		decision = svc.checkUnifiedFragments(context.Background(), nonGPTInput, runtime)
		require.True(t, decision.Allowed)
		require.False(t, decision.Flagged)
		require.Len(t, repo.snapshotLogs(), 1, "non-GPT traffic must have no local risk-control side effects")

		riskControlOff := *runtime
		riskControlOff.riskControlEnabled = false
		decision = svc.checkUnifiedFragments(context.Background(), input, &riskControlOff)
		require.True(t, decision.Allowed)
		require.False(t, decision.Flagged)
		require.Len(t, repo.snapshotLogs(), 1)
	})

	t.Run("second layer block and failure continue contract", func(t *testing.T) {
		blockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Safety: Unsafe\nCategories: jailbreak"}}]}`))
		}))
		defer blockUpstream.Close()

		failureUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "stub unavailable", http.StatusServiceUnavailable)
		}))
		defer failureUpstream.Close()

		cfg := defaultContentModerationConfig()
		cfg.Enabled = true
		cfg.Mode = ContentModerationModePreBlock
		cfg.AutoBanEnabled = false
		cfg.KeywordBlockingMode = ContentModerationKeywordModeAPIOnly
		cfg.SecondLayerEnabled = true
		cfg.SecondLayerScanners = []string{"jailbreak"}
		cfg.SecondLayerEndpoints = []ContentModerationEndpoint{{
			ID: "stub-block", Name: "stub block", BaseURL: blockUpstream.URL,
			Model: "stub-guard", Enabled: true, TimeoutMS: 1_000, InputLimit: 4_096,
		}}
		scope := NewContentModerationScopeSnapshot(nil, " GPT-acceptance ")
		input := ContentModerationCheckInput{
			Body: []byte(`{"input":"evaluate this request"}`), Scope: &scope,
			Protocol: ContentModerationProtocolOpenAIResponses,
		}
		repo := &contentModerationTestRepo{}
		svc := &ContentModerationService{repo: repo}
		runtime := &contentModerationRuntimeSnapshot{riskControlEnabled: true, config: cfg}

		blocked := svc.checkUnifiedFragments(context.Background(), input, runtime)
		require.True(t, blocked.Blocked)
		require.Equal(t, ContentModerationActionSecondLayerBlock, blocked.Action)
		require.Len(t, repo.snapshotLogs(), 1)

		failureCfg := cloneContentModerationConfig(cfg)
		failureCfg.SecondLayerEndpoints = []ContentModerationEndpoint{{
			ID: "stub-failure", Name: "stub failure", BaseURL: failureUpstream.URL,
			Model: "stub-guard", Enabled: true, TimeoutMS: 1_000, InputLimit: 4_096,
		}}
		continued := svc.checkUnifiedFragments(context.Background(), input, &contentModerationRuntimeSnapshot{
			riskControlEnabled: true, config: failureCfg,
		})
		require.True(t, continued.Allowed, "a failed second layer must continue to the real upstream")
		require.False(t, continued.Flagged)
		require.Len(t, repo.snapshotLogs(), 1, "second-layer infrastructure failures must not create violations")
	})

	t.Run("GPT cyber disposition and non GPT passthrough", func(t *testing.T) {
		regularRepo := &cyberDispositionTestRepo{userActive: true}
		regularInvalidator := &contentModerationTestAuthCacheInvalidator{}
		regularService := NewContentModerationService(
			&contentModerationTestSettingRepo{values: map[string]string{
				SettingKeyRiskControlEnabled:      "false",
				SettingKeyContentModerationConfig: `{"auto_ban_enabled":false,"email_on_hit":false}`,
			}},
			regularRepo, nil, nil, nil, nil, regularInvalidator, nil,
		)
		regularService.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
			UserID: 101, UserEmail: "acceptance@example.invalid", UserRole: RoleUser,
			Scope: gptCyberScope(), Model: "gpt-5.6", Endpoint: "/v1/responses",
			UpstreamMessage: "cyber policy", UpstreamStatus: http.StatusBadRequest,
		})
		regularLogs := regularRepo.snapshotLogs()
		require.Len(t, regularLogs, 1)
		require.Equal(t, "user", regularLogs[0].DispositionTarget)
		require.Equal(t, "disabled", regularLogs[0].DispositionStatus)
		require.Equal(t, []int64{101}, regularInvalidator.userIDs)

		adminRepo := &cyberDispositionTestRepo{userActive: true, keyActive: true, keyCredential: "sk-test-admin-triggering-key"}
		adminInvalidator := &contentModerationTestAuthCacheInvalidator{}
		adminService := NewContentModerationService(
			&contentModerationTestSettingRepo{values: map[string]string{SettingKeyRiskControlEnabled: "true"}},
			adminRepo, nil, nil, nil, nil, adminInvalidator, nil,
		)
		adminService.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
			UserID: 1, APIKeyID: 202, UserRole: RoleAdmin, Scope: gptCyberScope(),
			Model: "gpt-5.6", Endpoint: "/v1/responses", UpstreamMessage: "cyber policy",
		})
		adminLogs := adminRepo.snapshotLogs()
		require.Len(t, adminLogs, 1)
		require.Equal(t, "api_key", adminLogs[0].DispositionTarget)
		require.Equal(t, "disabled", adminLogs[0].DispositionStatus)
		require.Equal(t, 0, adminRepo.disableUserCalls)
		require.Equal(t, []string{"sk-test-admin-triggering-key"}, adminInvalidator.keys)

		nonGPTRepo := &cyberDispositionTestRepo{userActive: true}
		nonGPTService := NewContentModerationService(nil, nonGPTRepo, nil, nil, nil, nil, nil, nil)
		nonGPTScope := NewContentModerationScopeSnapshot(nil, "Claude production")
		nonGPTService.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
			UserID: 303, UserRole: RoleUser, Scope: &nonGPTScope,
			Model: "gpt-5.6", Endpoint: "/v1/responses", UpstreamMessage: "cyber policy",
		})
		require.Empty(t, nonGPTRepo.snapshotLogs())
		require.Zero(t, nonGPTRepo.disableUserCalls)
	})

	t.Run("raw HTTP SSE and WebSocket archives are exact", func(t *testing.T) {
		root := t.TempDir()
		keyRingPath := filepath.Join(root, "keyring.json")
		writeModerationArchiveTestKeyRing(t, keyRingPath, "acceptance-k1", map[string][]byte{
			"acceptance-k1": []byte("0123456789abcdef0123456789abcdef"),
		})
		repo := &moderationArchiveRuntimeTestRepo{}
		runtime, err := newContentModerationArchiveRuntime(repo, moderationArchiveRuntimeOptions(root, keyRingPath))
		require.NoError(t, err)
		t.Cleanup(runtime.Close)
		svc := &ContentModerationService{archiveRuntime: runtime}

		cases := []struct {
			name      string
			transport string
			stage     string
			target    string
			body      []byte
		}{
			{name: "http", transport: "http", stage: "http", target: "/v1/responses?stream=false", body: []byte("{\"input\":\"http\\u0000body\"}")},
			{name: "sse", transport: "http", stage: "http", target: "/v1/responses?stream=true", body: []byte("{\"input\":\"sse body\",\"stream\":true}")},
			{name: "websocket", transport: "websocket", stage: "subsequent_turn", target: "/v1/responses", body: []byte("{\"type\":\"response.create\",\"input\":\"ws turn\"}")},
		}
		for index, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				headers := http.Header{
					"Authorization": {"Bearer acceptance-only"},
					"Cookie":        {"first=fake", "second=fake"},
					"X-Repeated":    {"one", "two"},
				}
				input := ContentModerationCheckInput{RawRequest: ContentModerationRawRequest{
					Method: "POST", Target: tc.target, Headers: headers, Body: tc.body,
					Transport: tc.transport, Stage: tc.stage,
				}}
				log := &ContentModerationLog{Action: ContentModerationActionKeywordBlock, InputHash: "acceptance-hash"}
				require.NoError(t, svc.persistContentModerationArchive(context.Background(), log, input))

				_, archives := repo.snapshot()
				require.Len(t, archives, index+1)
				plaintext, err := runtime.cipher.Decrypt(&archives[index])
				require.NoError(t, err)
				var envelope ContentModerationArchiveEnvelope
				require.NoError(t, json.Unmarshal(plaintext, &envelope))
				decodedBody, err := base64.StdEncoding.DecodeString(envelope.Request.BodyBase64)
				require.NoError(t, err)
				require.Equal(t, tc.body, decodedBody)
				require.Equal(t, tc.target, envelope.Request.Target)
				require.Equal(t, tc.transport, envelope.Request.Transport)
				require.Equal(t, tc.stage, envelope.Request.Stage)
				require.Equal(t, []string{"one", "two"}, envelope.Request.Headers.Values("X-Repeated"))
				require.Equal(t, []string{"first=fake", "second=fake"}, envelope.Request.Headers.Values("Cookie"))
				require.Equal(t, "Bearer acceptance-only", envelope.Request.Headers.Get("Authorization"))

				broken := cloneModerationEncryptedArchive(&archives[index])
				broken.Chunks[0].Ciphertext[0] ^= 0xff
				_, err = runtime.cipher.Decrypt(broken)
				require.ErrorIs(t, err, ErrModerationArchiveIntegrity)
			})
		}
	})

	t.Run("pending body budget rejects concurrent overflow and recovers", func(t *testing.T) {
		const (
			workers = 16
			bytes   = int64(3)
			limit   = int64(12)
		)
		budget := NewContentModerationPendingBodyBudget()
		start := make(chan struct{})
		release := make(chan struct{})
		results := make(chan bool, workers)
		var wg sync.WaitGroup
		wg.Add(workers)
		for range workers {
			go func() {
				defer wg.Done()
				<-start
				reservation, ok := budget.TryReserve(bytes, limit)
				results <- ok
				if !ok {
					return
				}
				<-release
				reservation.Release()
			}()
		}
		close(start)
		succeeded := 0
		for range workers {
			if <-results {
				succeeded++
			}
		}
		require.Equal(t, 4, succeeded)
		require.Equal(t, limit, budget.InUse())
		require.Equal(t, limit, budget.MaxSeen())
		require.EqualValues(t, workers-succeeded, budget.Rejections())
		close(release)
		wg.Wait()
		require.Zero(t, budget.InUse())
	})

	t.Run("archive access and ordinary surfaces isolate secrets", func(t *testing.T) {
		const secret = "sk-proj-acceptance-secret-1234567890abcdef"
		rawBody := append([]byte(`{"authorization":"Bearer `+secret+`","padding":"`), bytes.Repeat([]byte("x"), ContentModerationArchivePreviewMaxBytes)...)
		rawBody = append(rawBody, []byte(`"}`)...)
		envelope := ContentModerationArchiveEnvelope{
			ArchiveID: "acceptance-sensitive-archive", Version: ContentModerationArchiveVersion,
			Request: ContentModerationArchiveRequest{
				Method: "POST", Target: "/v1/responses?stream=true",
				Headers: http.Header{
					"Authorization": {"Bearer " + secret},
					"Cookie":        {"session=" + secret, "preference=test"},
				},
				BodyBase64: base64.StdEncoding.EncodeToString(rawBody), Transport: "http", Stage: "http",
			},
			Action: ContentModerationActionKeywordBlock,
		}
		plaintext, err := json.Marshal(envelope)
		require.NoError(t, err)
		svc, repo, _ := newModerationArchiveServiceFixture(t, plaintext)
		svc.settingRepo = &contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled: "true",
		}}

		preview, err := svc.PreviewArchive(context.Background(), 77, 9, "acceptance-preview")
		require.NoError(t, err)
		require.True(t, preview.Truncated)
		require.Equal(t, int64(ContentModerationArchivePreviewMaxBytes), preview.ReturnedBytes)
		require.Equal(t, int64(len(rawBody)), preview.TotalBytes)
		require.Contains(t, preview.Content, secret)
		download, err := svc.DownloadArchive(context.Background(), 77, 9, "acceptance-download")
		require.NoError(t, err)
		var exported struct {
			Request struct {
				Headers http.Header     `json:"headers"`
				Body    json.RawMessage `json:"body"`
			} `json:"request"`
		}
		require.NoError(t, json.Unmarshal(download, &exported))
		require.JSONEq(t, string(rawBody), string(exported.Request.Body))
		require.Contains(t, string(exported.Request.Body), secret)
		require.Equal(t, []string{"session=" + secret, "preference=test"}, exported.Request.Headers.Values("Cookie"))

		cfg := defaultContentModerationConfig()
		diagnostic := svc.buildLog(
			ContentModerationCheckInput{}, cfg, ContentModerationActionKeywordBlock, true,
			"jailbreak", 1, map[string]float64{"jailbreak": 1}, "Bearer "+secret, nil, nil, "",
		)
		diagnostic.UserEmail = "acceptance@example.invalid"
		diagnostic.GroupName = "GPT acceptance"
		diagnostic.ViolationCount = 1
		ordinaryLog, err := json.Marshal(diagnostic)
		require.NoError(t, err)
		email := buildContentModerationAccountDisabledEmailBody("Acceptance", diagnostic, cfg)
		status, err := svc.GetStatus(context.Background())
		require.NoError(t, err)
		metrics, err := json.Marshal(status)
		require.NoError(t, err)
		for name, surface := range map[string]string{
			"ordinary log and list model": string(ordinaryLog),
			"email":                       email,
			"metrics":                     string(metrics),
		} {
			require.NotContains(t, surface, secret, "surface=%s", name)
		}
		require.Contains(t, diagnostic.InputExcerpt, "[已脱敏]")

		deleted, err := svc.DeleteArchive(context.Background(), 77, 9, "acceptance-delete")
		require.NoError(t, err)
		require.True(t, deleted)
		audits := repo.snapshotAudits()
		require.Len(t, audits, 3)
		require.Equal(t, []string{"preview", "download", "delete"}, []string{audits[0].Action, audits[1].Action, audits[2].Action})
		_, err = svc.DownloadArchive(context.Background(), 77, 9, "acceptance-after-delete")
		require.Error(t, err)
		require.NotContains(t, strings.ToLower(err.Error()), strings.ToLower(secret))
	})

	t.Run("session cache rewrite and edge challenge contracts", func(t *testing.T) {
		metadata := "user_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2_account__session_123e4567-e89b-12d3-a456-426614174000"
		sessionContext := &SessionContext{ClientIP: "192.0.2.10", UserAgent: "claude-cli/2.1.87", APIKeyID: 408, ClientSessionID: "client-session-acceptance"}
		parsed := &ParsedRequest{MetadataUserID: metadata, SessionContext: sessionContext}
		require.Equal(t, "client-session-acceptance", (&GatewayService{}).GenerateSessionHash(parsed))

		account := &Account{ID: 22094, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{}}
		require.False(t, account.IsAnthropicAPIKeyCacheControlRewriteEnabled())
		account.Extra[AnthropicAPIKeyCacheControlRewriteExtraKey] = true
		body := []byte(`{"metadata":{"user_id":"client-provided","other":"kept"},"messages":[{"role":"user","content":"hello"}]}`)
		injected := injectAnthropicAPIKeyCacheMetadata(body, parsed, account)
		require.Equal(t, "client-provided", gjson.GetBytes(injected, "metadata.user_id").String())
		require.Equal(t, "kept", gjson.GetBytes(injected, "metadata.other").String())
		rewritten := rewriteMessageCacheControlBody(injected)
		require.Equal(t, claude.DefaultCacheControlTTL, gjson.GetBytes(rewritten, "messages.0.content.0.cache_control.ttl").String())

		require.True(t, isOpenAIEdgeChallenge403([]byte("<!doctype html><title>Just a moment...</title>")))
		require.False(t, isOpenAIEdgeChallenge403([]byte(`{"error":{"message":"workspace forbidden"}}`)))
	})
}
