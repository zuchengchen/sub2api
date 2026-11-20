package service

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Read the capability declared on this attempt. An explicit HTTP header takes
// precedence over frame metadata, and the GPT-5.5 mapping always uses standard
// Responses. A caller validating a physical Lite payload must still reject
// hosted search history if the gateway has left the marker on it.
func isOpenAIResponsesLiteFinalRequest(account *Account, finalModel, liteHeader string, body []byte) bool {
	if account == nil || !account.IsOpenAIOAuthLike() || strings.TrimSpace(finalModel) == "gpt-5.5" {
		return false
	}
	if strings.TrimSpace(liteHeader) != "" {
		return isOpenAIResponsesLiteHeader(liteHeader)
	}
	return isOpenAIResponsesLiteWebSocketPayload(body)
}

// Hosted search output needs a corresponding hosted tool declaration even
// when the client only wants to replay history. Lite cannot carry that pair.
func openAIResponsesMustUseStandard(account *Account, model string, body []byte) bool {
	return account.IsOpenAIOAuthLike() && (strings.TrimSpace(model) == "gpt-5.5" || openAIResponsesHasWebSearchHistory(body))
}

func removeOpenAIResponsesLiteMetadata(body []byte) ([]byte, error) {
	const path = "client_metadata." + responsesLiteWSMetadataKey
	if !gjson.GetBytes(body, path).Exists() {
		return body, nil
	}
	out, err := sjson.DeleteBytes(body, path)
	if err != nil {
		return body, fmt.Errorf("remove Responses Lite metadata: %w", err)
	}
	return out, nil
}

// Apply after the final HTTP header projection. Preserve the inbound request
// for other account attempts and keep GetBody in sync for transport retries.
func applyOpenAIStandardResponsesCompatibility(req *http.Request, account *Account, body []byte) error {
	if req == nil || !account.IsOpenAIOAuthLike() {
		return nil
	}
	if req.Header.Get(responsesLiteHeader) == "" && !gjson.GetBytes(body, "client_metadata."+responsesLiteWSMetadataKey).Exists() {
		return nil
	}
	if !openAIResponsesMustUseStandard(account, gjson.GetBytes(body, "model").String(), body) {
		return nil
	}
	out, err := removeOpenAIResponsesLiteMetadata(body)
	if err != nil {
		return err
	}
	if len(out) != len(body) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		req.Body = io.NopCloser(bytes.NewReader(out))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(out)), nil }
		req.ContentLength = int64(len(out))
		if req.Header.Get("Content-Length") != "" {
			req.Header.Set("Content-Length", strconv.Itoa(len(out)))
		}
	}
	req.Header.Del(responsesLiteHeader)
	return nil
}
