package service

import "context"

// Realtime uses default WebSocket extensions; Responses explicitly opts into
// permessage-deflate. Keep that protocol choice scoped to the dial context.
type openAIWSCompressionDisabledKey struct{}

func withoutOpenAIWSCompression(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAIWSCompressionDisabledKey{}, true)
}

func openAIWSCompressionDisabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	disabled, _ := ctx.Value(openAIWSCompressionDisabledKey{}).(bool)
	return disabled
}

type liveSidebandHandshakeError struct {
	status int
	err    error
}

func (e *liveSidebandHandshakeError) Error() string { return e.err.Error() }
func (e *liveSidebandHandshakeError) Unwrap() error { return e.err }
