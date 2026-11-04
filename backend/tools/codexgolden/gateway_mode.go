//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func validateGatewayIngressMode(mode string) error {
	if mode != "passthrough" && mode != "ctx_pool" {
		return errors.New("ingress-mode must be passthrough or ctx_pool")
	}
	return nil
}
func configureGatewayIngressMode(cfg *config.Config, account *service.Account, mode string) error {
	if err := validateGatewayIngressMode(mode); err != nil {
		return err
	}
	if cfg == nil || account == nil {
		return errors.New("missing gateway mode configuration")
	}
	cfg.Gateway.OpenAIWS.IngressModeDefault = mode
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
	return verifyGatewayModeResolution(cfg, account, mode)
}
func verifyGatewayModeResolution(cfg *config.Config, account *service.Account, mode string) error {
	if err := validateGatewayIngressMode(mode); err != nil {
		return err
	}
	if cfg == nil || account == nil || !cfg.Gateway.OpenAIWS.ModeRouterV2Enabled {
		return errors.New("gateway mode router is unavailable")
	}
	if account.ResolveOpenAIResponsesWebSocketV2Mode(cfg.Gateway.OpenAIWS.IngressModeDefault) != mode {
		return errors.New("requested ingress mode does not match account resolution")
	}
	if service.NewOpenAIWSProtocolResolver(cfg).Resolve(account).Transport != service.OpenAIUpstreamTransportResponsesWebsocketV2 {
		return errors.New("requested ingress mode is not an active WS-v2 route")
	}
	return nil
}

// Candidate-only hash: unlike recordingConfigHash, this binds the product
// mode/config and is never compared against or copied into official reference.
func gatewayConfigFingerprint(cfg *config.Config) string {
	return fmt.Sprintf("%x", sha256.Sum256(mustJSON(cfg.Gateway)))
}

type gatewayIngressObservation struct {
	Sequence         int    `json:"sequence"`
	ActualMode       string `json:"actual_ingress_mode"`
	FallbackReason   string `json:"fallback_reason,omitempty"`
	Upgrade426Before int64  `json:"upgrade_426_before"`
	Upgrade426After  int64  `json:"upgrade_426_after"`
	HTTPResponses    int64  `json:"upstream_http_responses"`

	AccountMode        string `json:"account_mode"`
	ResolverTransport  string `json:"resolver_transport"`
	PoolAcquiresBefore int64  `json:"pool_acquires_before"`
	PoolAcquiresAfter  int64  `json:"pool_acquires_after"`
}
type gatewayModeEvidence struct {
	Scenario            string `json:"scenario"`
	Upgrade426Responses int64  `json:"upstream_upgrade_426_responses"`

	Mode         string                      `json:"candidate_ingress_mode"`
	ConfigHash   string                      `json:"gateway_config_sha256"`
	Observations []gatewayIngressObservation `json:"ingress_observations"`
}

func validateGatewayModeEvidence(meta captureMetadata, evidence gatewayModeEvidence, wsRoutes int) error {
	if err := validateGatewayIngressMode(meta.GatewayMode); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(meta.GatewayConfigHash) {
		return errors.New("missing gateway configuration fingerprint")
	}
	if evidence.Mode != meta.GatewayMode || evidence.ConfigHash != meta.GatewayConfigHash {
		return errors.New("candidate ingress mode/config evidence mismatch")
	}
	if wsRoutes <= 0 || len(evidence.Observations) != wsRoutes {
		return errors.New("missing actual WS ingress observations")
	}
	seen := map[int]bool{}
	for _, o := range evidence.Observations {
		if o.Sequence <= 0 || seen[o.Sequence] {
			return errors.New("missing or duplicate ingress sequence")
		}
		seen[o.Sequence] = true
		if o.AccountMode != meta.GatewayMode || o.ResolverTransport != "responses_websockets_v2" {
			return errors.New("actual account/resolver mode disagrees with requested ingress")
		}
		if o.PoolAcquiresBefore < 0 || o.PoolAcquiresAfter < o.PoolAcquiresBefore || o.Upgrade426Before < 0 || o.Upgrade426After < o.Upgrade426Before || o.Upgrade426After > evidence.Upgrade426Responses || o.HTTPResponses < 0 {
			return errors.New("invalid ingress execution counters")
		}
		delta := o.PoolAcquiresAfter - o.PoolAcquiresBefore
		if o.ActualMode == "http_bridge" {
			if (evidence.Scenario != "http_fallback_tool_loop" && evidence.Scenario != "http_unauthorized_refresh") || o.FallbackReason != "prior_upstream_426" || o.Upgrade426Before <= 0 || o.HTTPResponses <= 0 || delta != 0 {
				return errors.New("HTTP bridge lacks positive upstream rejection/request evidence")
			}
			prior := false
			for _, earlier := range evidence.Observations {
				if earlier.Sequence < o.Sequence && earlier.Upgrade426After > earlier.Upgrade426Before && earlier.Upgrade426After <= o.Upgrade426Before {
					prior = true
				}
			}
			if !prior {
				return errors.New("HTTP bridge has no preceding observed 426 ingress")
			}
			continue
		}
		if o.ActualMode != meta.GatewayMode || o.FallbackReason != "" || o.HTTPResponses != 0 {
			return errors.New("unverified actual ingress mode")
		}
		if meta.GatewayMode == "ctx_pool" && delta <= 0 {
			return errors.New("ctx_pool entry did not execute a pool acquire")
		}
		if meta.GatewayMode == "passthrough" && delta != 0 {
			return errors.New("passthrough entry unexpectedly used the pool")
		}
	}
	return nil
}
func readGatewayModeEvidence(dir, scenario string, meta captureMetadata) error {
	data, err := os.ReadFile(filepath.Join(dir, scenario+"-provenance.json"))
	if err != nil {
		return err
	}
	var provenance struct {
		gatewayModeEvidence
		Routes map[string]int `json:"service_routes"`
	}
	if err := json.Unmarshal(data, &provenance); err != nil {
		return err
	}
	if provenance.Scenario != scenario {
		return errors.New("gateway scenario identity mismatch")
	}
	if err := validateGatewayModeEvidence(meta, provenance.gatewayModeEvidence, provenance.Routes["WS /backend-api/codex/responses"]); err != nil {
		return err
	}
	rows, err := readRecordMap(filepath.Join(dir, scenario+".jsonl"))
	if err != nil {
		return err
	}
	var records []record
	for _, row := range rows {
		records = append(records, row)
	}
	return validateGatewayModeWireEvidence(provenance.gatewayModeEvidence, records)
}

func validateGatewayModeWireEvidence(evidence gatewayModeEvidence, records []record) error {
	var httpRequests, handshakes int64
	for _, row := range records {
		if row.Hop != "gateway_upstream" {
			continue
		}
		if row.Kind == "http_request" && row.Method == "POST" && row.Target == "/backend-api/codex/responses" {
			httpRequests++
		}
		if row.Kind == "ws_handshake" && row.Target == "/backend-api/codex/responses" {
			handshakes++
		}
	}
	return validateGatewayModeWireCounts(evidence, httpRequests, handshakes)
}
func validateGatewayModeWireCounts(evidence gatewayModeEvidence, httpRequests, handshakes int64) error {
	var responses int64
	for _, o := range evidence.Observations {
		responses += o.HTTPResponses
	}
	if responses > httpRequests || evidence.Upgrade426Responses > handshakes {
		return errors.New("ingress fallback counters exceed actual gateway socket capture")
	}
	if evidence.Upgrade426Responses > 0 && (evidence.Scenario != "http_fallback_tool_loop" && evidence.Scenario != "http_unauthorized_refresh") {
		return errors.New("unexpected upstream 426 scenario")
	}
	return nil
}
