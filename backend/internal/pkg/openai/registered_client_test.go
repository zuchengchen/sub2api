package openai

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type keyPouchTestTransport func(*http.Request) (*http.Response, error)

func (rt keyPouchTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return rt(req) }

func TestKeyPouchPublicTokenTransport(t *testing.T) {
	for _, status := range []int{200, 307, 400} {
		calls := 0
		client := &http.Client{Transport: keyPouchTestTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			require.Equal(t, RegisteredTokenURL, r.URL.String())
			require.Equal(t, http.MethodPost, r.Method)
			require.NoError(t, r.ParseForm())
			require.Equal(t, "test-verifier", r.PostForm.Get("code_verifier"))
			require.Equal(t, "test-code", r.PostForm.Get("code"))
			require.Equal(t, "public-client", r.PostForm.Get("client_id"))
			require.Empty(t, r.Header.Get("Authorization"))
			body := `{"access_token":"test-access","token_type":"Bearer"}`
			headers := make(http.Header)
			if status != 200 {
				body = `{"error":"invalid_client","error_description":"never-log-the-upstream-body"}`
				headers.Set("Location", "https://other.example.invalid/leak")
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Request: r, Header: headers}, nil
		})}
		form := url.Values{"grant_type": {"authorization_code"}, "code": {"test-code"}, "code_verifier": {"test-verifier"}, "client_id": {"public-client"}}
		token, err := sendRegisteredTokenGrant(context.Background(), form, client)
		if status == 200 {
			require.NoError(t, err)
			require.Equal(t, "test-access", token.AccessToken)
		} else {
			require.Error(t, err)
			require.NotContains(t, err.Error(), "never-log")
			require.NotContains(t, err.Error(), "test-code")
		}
		require.Equal(t, 1, calls)
	}
}
