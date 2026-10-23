package service

import (
	"sync"
	"sync/atomic"
)

// The process-wide drain signal lets long-lived streams end cleanly when the
// server shuts down. http.Server.Shutdown does not cancel in-flight request
// contexts and only waits a few seconds, so a Grok reasoning stream that runs
// for tens of minutes would otherwise be cut mid-stream and the client would
// see ECONNRESET instead of a finished turn.
type gatewayDrainState struct {
	once sync.Once
	ch   chan struct{}
}

var gatewayDrain atomic.Pointer[gatewayDrainState]

func init() {
	gatewayDrain.Store(&gatewayDrainState{ch: make(chan struct{})})
}

// BeginGatewayDrain signals in-flight streams that the process is shutting
// down. Safe to call more than once.
func BeginGatewayDrain() {
	state := gatewayDrain.Load()
	state.once.Do(func() { close(state.ch) })
}

// GatewayDraining is closed once BeginGatewayDrain has been called.
func GatewayDraining() <-chan struct{} {
	return gatewayDrain.Load().ch
}
