package service

import (
	"time"

	"github.com/gin-gonic/gin"
)

// Passthrough may abandon an upstream response before its headers are sent,
// or a keepalive may commit them before semantic output. Observe the actual
// writer boundary so provenance describes precisely the header the client saw.
func (s *OpenAIGatewayService) watchOpenAICodexTurnStateCommit(c *gin.Context, account *Account) func() {
	if s == nil || c == nil || c.Writer == nil || account == nil || account.ID <= 0 {
		return func() {}
	}
	seed := openAICodexTurnStateSeed(c)
	if seed == "" || c.Writer.Written() {
		return func() {}
	}
	original := c.Writer
	origin := openAICodexTurnStateOrigin{accountID: account.ID, oauthOwner: openAIOAuthOwnerForAccount(account)}
	ttl := s.openAIWSSessionStickyTTL()
	wrapped := &openAICodexTurnStateCommitWriter{ResponseWriter: original, onCommit: func(state string) {
		origin.expiresAt = time.Now().Add(ttl)
		s.openaiCodexTurnStateOrigins.Store(openAICodexTurnStateOriginKey(seed, state), origin)
		s.sweepOpenAICodexTurnStateOrigins()
	}}
	c.Writer = wrapped
	return func() {
		if c.Writer == wrapped {
			c.Writer = original
		}
	}
}

type openAICodexTurnStateCommitWriter struct {
	gin.ResponseWriter
	onCommit func(string)
}

func (w *openAICodexTurnStateCommitWriter) pendingState() string {
	if w.ResponseWriter.Written() {
		return ""
	}
	return extractOpenAICodexTurnState(w.ResponseWriter.Header())
}

func (w *openAICodexTurnStateCommitWriter) committed(state string) {
	if state != "" && w.ResponseWriter.Written() {
		w.onCommit(state)
	}
}

func (w *openAICodexTurnStateCommitWriter) Write(data []byte) (int, error) {
	state := w.pendingState()
	n, err := w.ResponseWriter.Write(data)
	w.committed(state)
	return n, err
}

func (w *openAICodexTurnStateCommitWriter) WriteString(data string) (int, error) {
	state := w.pendingState()
	n, err := w.ResponseWriter.WriteString(data)
	w.committed(state)
	return n, err
}

func (w *openAICodexTurnStateCommitWriter) WriteHeader(code int) {
	state := w.pendingState()
	w.ResponseWriter.WriteHeader(code)
	w.committed(state)
}

func (w *openAICodexTurnStateCommitWriter) WriteHeaderNow() {
	state := w.pendingState()
	w.ResponseWriter.WriteHeaderNow()
	w.committed(state)
}

func (w *openAICodexTurnStateCommitWriter) Flush() {
	state := w.pendingState()
	w.ResponseWriter.Flush()
	w.committed(state)
}
