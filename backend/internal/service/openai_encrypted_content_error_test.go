package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const encryptedFunctionOutputFailed = `{"type":"response.failed","response":{"status":"failed","error":{"type":"server_error","code":"server_error","message":"Encrypted function output content could not be decrypted or decoded."}}}`

func TestIsOpenAIEncryptedContentError(t *testing.T) {
	for _, msg := range []string{
		"Encrypted function output content could not be decrypted or decoded.",
		"The encrypted content for item amsg_01a0e31b could not be verified. Reason: encrypted content hydration failed: Encrypted content could not be decrypted or parsed.",
		"invalid_encrypted_content",
		"encrypted content could not be verified",
	} {
		require.True(t, isOpenAIEncryptedContentError(msg, nil), msg)
	}
	require.True(t, isOpenAIEncryptedContentError("", []byte(encryptedFunctionOutputFailed)))
	require.True(t, isOpenAIEncryptedContentError("", []byte(`{"error":{"code":"invalid_encrypted_content","message":"bad"}}`)))
	require.True(t, isOpenAIEncryptedContentError("", []byte("Encrypted content could not be decrypted")), "plain-text body")

	for _, msg := range []string{
		"", "Upstream service temporarily unavailable", "An error occurred while processing your request.",
		"Your input exceeds the context window of this model.", "could not be decrypted", "encrypted",
	} {
		require.False(t, isOpenAIEncryptedContentError(msg, nil), msg)
	}
	// Echoed request content in a structured body must not change the class.
	require.False(t, isOpenAIEncryptedContentError("", []byte(`{"error":{"message":"temporary outage"},"echo":"encrypted content could not be decrypted"}`)))
}

func TestOpenAIEncryptedContentErrorSkipsFailover(t *testing.T) {
	payload := []byte(encryptedFunctionOutputFailed)
	message := extractOpenAISSEErrorMessage(payload)
	require.False(t, openAIStreamFailedEventShouldFailover(payload, message))
	require.False(t, openAIStreamErrorEventShouldFailover(payload, message))
	svc := &OpenAIGatewayService{}
	require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(newOpenAIUpstreamErrorTestAccount(), http.StatusBadGateway, "",
		[]byte(`{"error":{"message":"Encrypted function output content could not be decrypted or decoded.","type":"server_error"}}`)))

	// Other server errors keep failing over.
	generic := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"type":"server_error","code":"server_error","message":"An error occurred while processing your request."}}}`)
	require.True(t, openAIStreamFailedEventShouldFailover(generic, extractOpenAISSEErrorMessage(generic)))
	require.True(t, svc.shouldFailoverOpenAIUpstreamResponse(newOpenAIUpstreamErrorTestAccount(), http.StatusBadGateway, "temporary upstream outage",
		[]byte(`{"error":{"message":"temporary upstream outage"}}`)))
}

func TestOpenAIStreamingEncryptedContentFailedBeforeOutputPassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			"event: response.failed",
			"data: " + encryptedFunctionOutputFailed,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-encrypted-content"}},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "model", "model")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "must not switch accounts")
	require.Contains(t, rec.Body.String(), "could not be decrypted or decoded")
}

func TestAccountHealthExcludesEncryptedContentErrors(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(regexp.QuoteMeta(`~* $3::text`)).
		WithArgs(10, openAICookieWSUnavailableMessage, openAIEncryptedContentErrorPattern).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "platform", "ok", "avg_ms", "err", "until", "reason", "min_group_live"}))
	svc := NewAccountHealthService(db, nil, nil)
	_, err = svc.queryWindowStats(context.Background(), 10)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	// The SQL applies the same pattern case-insensitively (~*), like Go's (?i).
	sqlLike := regexp.MustCompile(`(?i)` + openAIEncryptedContentErrorPattern)
	require.True(t, sqlLike.MatchString("Encrypted function output content could not be decrypted or decoded. Upstream service temporarily unavailable"))
	require.False(t, sqlLike.MatchString("Upstream service temporarily unavailable"))
}
