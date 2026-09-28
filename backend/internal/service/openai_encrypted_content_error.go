package service

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// openAIEncryptedContentErrorPattern matches upstream rejections of encrypted
// items (reasoning, function output) that the serving account cannot decrypt,
// e.g. "Encrypted function output content could not be decrypted or decoded."
// or "The encrypted content for item ... could not be verified". The encrypted
// payload belongs to whichever account minted it, so the error follows the
// request: switching accounts cannot fix it and it says nothing about account
// health. Lowercase POSIX/RE2-compatible: Go compiles it with (?i) and the
// account health SQL applies it with ~*.
const openAIEncryptedContentErrorPattern = `invalid_encrypted_content|encrypted [a-z ]*content[^.]* could not be (decrypted|decoded|verified)`

var openAIEncryptedContentErrorRE = regexp.MustCompile(`(?i)` + openAIEncryptedContentErrorPattern)

// isOpenAIEncryptedContentError reports a request-scoped encrypted-content
// rejection. Structured bodies are inspected only on known error fields so
// echoed request content cannot change the classification.
func isOpenAIEncryptedContentError(upstreamMsg string, upstreamBody []byte) bool {
	if openAIEncryptedContentErrorRE.MatchString(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	if !gjson.ValidBytes(upstreamBody) {
		return openAIEncryptedContentErrorRE.MatchString(string(upstreamBody))
	}
	for _, path := range []string{
		"error.message", "response.error.message", "message",
		"error.code", "response.error.code", "code",
	} {
		if value := strings.TrimSpace(gjson.GetBytes(upstreamBody, path).String()); value != "" && openAIEncryptedContentErrorRE.MatchString(value) {
			return true
		}
	}
	return false
}
