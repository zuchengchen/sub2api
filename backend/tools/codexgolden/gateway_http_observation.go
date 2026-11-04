//go:build linux

package main

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type gatewayIngressHTTPKey struct{}
type gatewayIngressHTTPObservation struct{ responses atomic.Int64 }
type gatewayObservedHTTPUpstream struct{ service.HTTPUpstream }

func observeGatewayIngressHTTP(request *http.Request, response *http.Response) {
	if request == nil || request.URL == nil || request.Method != "POST" || request.URL.Path != "/backend-api/codex/responses" || response == nil {
		return
	}
	if observer, ok := request.Context().Value(gatewayIngressHTTPKey{}).(*gatewayIngressHTTPObservation); ok {
		observer.responses.Add(1)
	}
}
func (u gatewayObservedHTTPUpstream) Do(request *http.Request, proxy string, accountID int64, concurrency int) (*http.Response, error) {
	response, err := u.HTTPUpstream.Do(request, proxy, accountID, concurrency)
	observeGatewayIngressHTTP(request, response)
	return response, err
}
func (u gatewayObservedHTTPUpstream) DoWithTLS(request *http.Request, proxy string, accountID int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	response, err := u.HTTPUpstream.DoWithTLS(request, proxy, accountID, concurrency, profile)
	observeGatewayIngressHTTP(request, response)
	return response, err
}
func withGatewayIngressHTTPObservation(request *http.Request) (*http.Request, *gatewayIngressHTTPObservation) {
	observer := &gatewayIngressHTTPObservation{}
	return request.WithContext(context.WithValue(request.Context(), gatewayIngressHTTPKey{}, observer)), observer
}
