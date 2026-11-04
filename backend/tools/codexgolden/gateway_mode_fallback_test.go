//go:build linux

package main

import (
	"strings"
	"testing"
)

func fallbackModeFixture() (captureMetadata, gatewayModeEvidence) {
	hash := strings.Repeat("a", 64)
	return captureMetadata{GatewayMode: "ctx_pool", GatewayConfigHash: hash}, gatewayModeEvidence{
		Scenario: "http_fallback_tool_loop", Mode: "ctx_pool", ConfigHash: hash, Upgrade426Responses: 1,
		Observations: []gatewayIngressObservation{
			{Sequence: 1, ActualMode: "ctx_pool", AccountMode: "ctx_pool", ResolverTransport: "responses_websockets_v2", PoolAcquiresBefore: 0, PoolAcquiresAfter: 1, Upgrade426Before: 0, Upgrade426After: 1},
			{Sequence: 2, ActualMode: "http_bridge", FallbackReason: "prior_upstream_426", AccountMode: "ctx_pool", ResolverTransport: "responses_websockets_v2", PoolAcquiresBefore: 1, PoolAcquiresAfter: 1, Upgrade426Before: 1, Upgrade426After: 1, HTTPResponses: 2},
		},
	}
}
func TestGatewayModeFallbackAcceptsObserved426ThenHTTP(t *testing.T) {
	meta, evidence := fallbackModeFixture()
	if err := validateGatewayModeEvidence(meta, evidence, 2); err != nil {
		t.Fatal(err)
	}
	if err := validateGatewayModeWireCounts(evidence, 2, 1); err != nil {
		t.Fatal(err)
	}
}
func TestGatewayModeFallbackRejectsMissingOrUnrelatedWork(t *testing.T) {
	for _, fault := range []string{"no http", "no prior 426", "fake reason", "wrong scenario", "no prior ingress rejected", "duplicate sequence", "pool unused normal ctx", "mixed acquire and bridge", "no wire http", "no wire handshake"} {
		t.Run(fault, func(t *testing.T) {
			meta, evidence := fallbackModeFixture()
			wireHTTP, wireWS := int64(2), int64(1)
			switch fault {
			case "no http":
				evidence.Observations[1].HTTPResponses = 0
			case "no prior 426":
				evidence.Observations[1].Upgrade426Before = 0
			case "fake reason":
				evidence.Observations[1].FallbackReason = "trust_me_fallback"
			case "wrong scenario":
				evidence.Scenario = "ws_tool_loop"
			case "no prior ingress rejected":
				evidence.Observations[0].Upgrade426After = 0
			case "duplicate sequence":
				evidence.Observations[1].Sequence = 1
			case "pool unused normal ctx":
				evidence.Observations[1].ActualMode = "ctx_pool"
				evidence.Observations[1].FallbackReason = ""
				evidence.Observations[1].HTTPResponses = 0
			case "mixed acquire and bridge":
				evidence.Observations[1].PoolAcquiresAfter = 2
			case "no wire http":
				wireHTTP = 0
			case "no wire handshake":
				wireWS = 0
			}
			err := validateGatewayModeEvidence(meta, evidence, 2)
			if err == nil {
				err = validateGatewayModeWireCounts(evidence, wireHTTP, wireWS)
			}
			if err == nil {
				t.Fatal("invalid fallback evidence passed")
			}
		})
	}
}
func TestGatewayModeNonFallbackStillRequiresPoolAcquire(t *testing.T) {
	meta, evidence := fallbackModeFixture()
	evidence.Scenario = "ws_initial"
	evidence.Upgrade426Responses = 0
	evidence.Observations = evidence.Observations[:1]
	evidence.Observations[0].Upgrade426After = 0
	if err := validateGatewayModeEvidence(meta, evidence, 1); err != nil {
		t.Fatal(err)
	}
	evidence.Observations[0].PoolAcquiresAfter = 0
	if err := validateGatewayModeEvidence(meta, evidence, 1); err == nil {
		t.Fatal("non-fallback ctx_pool lost its positive pool requirement")
	}
}
