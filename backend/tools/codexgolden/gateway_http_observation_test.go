//go:build linux

package main

import (
	"context"
	"net/http"
	"testing"
)

func TestGatewayHTTPObservationIsCausalAndRequiresActualResponse(t *testing.T) {
	request, _ := http.NewRequest("POST", "https://chatgpt.com/backend-api/codex/responses", nil)
	first, a := withGatewayIngressHTTPObservation(request)
	second, b := withGatewayIngressHTTPObservation(request)
	child, cancel := context.WithCancel(first.Context())
	defer cancel()
	observeGatewayIngressHTTP(first.WithContext(child), &http.Response{StatusCode: 200})
	observeGatewayIngressHTTP(first, &http.Response{StatusCode: 401})
	observeGatewayIngressHTTP(second, &http.Response{StatusCode: 200})
	observeGatewayIngressHTTP(first, nil)
	observeGatewayIngressHTTP(request, &http.Response{StatusCode: 200})
	for _, target := range []string{"https://auth.openai.com/oauth/token", "https://chatgpt.com/backend-api/codex/models"} {
		other, _ := http.NewRequestWithContext(first.Context(), "POST", target, nil)
		observeGatewayIngressHTTP(other, &http.Response{StatusCode: 200})
	}
	if a.responses.Load() != 2 || b.responses.Load() != 1 {
		t.Fatalf("ingress causal counts differ: first=%d second=%d", a.responses.Load(), b.responses.Load())
	}
}
