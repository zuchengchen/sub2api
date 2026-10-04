package service

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAIClientSessionKindThread  = "thread"
	openAIClientSessionKindSession = "session"
)

var openAIThreadIdentityHeaders = []string{
	"conversation_id",
	"thread_id",
	"thread-id",
	codeBuddyConversationHeader,
}

var openAISessionIdentityHeaders = []string{
	"session_id",
	"session-id",
	openCodeSessionIDHeader,
	openCodeNativeSessionHeader,
}

type openAIClientSessionIdentity struct {
	kind  string
	value string
}

type openAIClientSessionIdentityStatus string

const (
	openAIClientSessionIdentityResolved openAIClientSessionIdentityStatus = "resolved"
	openAIClientSessionIdentityMissing  openAIClientSessionIdentityStatus = "missing"
	openAIClientSessionIdentityConflict openAIClientSessionIdentityStatus = "conflict"
	openAIClientSessionIdentityInvalid  openAIClientSessionIdentityStatus = "invalid"
)

type openAIClientSessionIdentityMetadata struct {
	Status openAIClientSessionIdentityStatus
	Kind   string
	Source string
}

type openAIClientSessionIdentityResolution struct {
	metadata openAIClientSessionIdentityMetadata
	identity openAIClientSessionIdentity
}

type openAIIdentityValue struct {
	value  string
	status openAIClientSessionIdentityStatus
}

func resolveOpenAIClientSessionIdentity(c *gin.Context, body []byte) openAIClientSessionIdentityResolution {
	if c == nil || c.Request == nil {
		return missingOpenAIClientSessionIdentity()
	}

	view := openAIRequestPayloadView(body)
	bodyThread, invalidBodyThread := openAIClientMetadataIdentity(view, "client_metadata.thread_id")
	bodySession, invalidBodySession := openAIClientMetadataIdentity(view, "client_metadata.session_id")

	headerThread := openAIIdentityHeader(c, openAIThreadIdentityHeaders)
	if headerThread.status == openAIClientSessionIdentityInvalid || invalidBodyThread {
		return rejectedOpenAIClientSessionIdentity(openAIClientSessionIdentityInvalid, openAIClientSessionKindThread, identityFailureSource(headerThread, invalidBodyThread))
	}
	if headerThread.status == openAIClientSessionIdentityConflict || openAIIdentityValuesConflict(headerThread.value, bodyThread) {
		return rejectedOpenAIClientSessionIdentity(openAIClientSessionIdentityConflict, openAIClientSessionKindThread, identityConflictSource(headerThread, bodyThread))
	}
	if headerThread.value != "" {
		source := "header"
		if bodyThread != "" {
			source = "header_body"
		}
		return resolvedOpenAIClientSessionIdentity(openAIClientSessionKindThread, headerThread.value, source)
	}
	if bodyThread != "" {
		return resolvedOpenAIClientSessionIdentity(openAIClientSessionKindThread, bodyThread, "body")
	}

	headerSession := openAIIdentityHeader(c, openAISessionIdentityHeaders)
	if headerSession.status == openAIClientSessionIdentityInvalid || invalidBodySession {
		return rejectedOpenAIClientSessionIdentity(openAIClientSessionIdentityInvalid, openAIClientSessionKindSession, identityFailureSource(headerSession, invalidBodySession))
	}
	if headerSession.status == openAIClientSessionIdentityConflict || openAIIdentityValuesConflict(headerSession.value, bodySession) {
		return rejectedOpenAIClientSessionIdentity(openAIClientSessionIdentityConflict, openAIClientSessionKindSession, identityConflictSource(headerSession, bodySession))
	}
	if headerSession.value != "" {
		source := "header"
		if bodySession != "" {
			source = "header_body"
		}
		return resolvedOpenAIClientSessionIdentity(openAIClientSessionKindSession, headerSession.value, source)
	}
	if bodySession != "" {
		return resolvedOpenAIClientSessionIdentity(openAIClientSessionKindSession, bodySession, "body")
	}
	return missingOpenAIClientSessionIdentity()
}

func missingOpenAIClientSessionIdentity() openAIClientSessionIdentityResolution {
	return openAIClientSessionIdentityResolution{metadata: openAIClientSessionIdentityMetadata{
		Status: openAIClientSessionIdentityMissing,
		Source: "none",
	}}
}

func rejectedOpenAIClientSessionIdentity(status openAIClientSessionIdentityStatus, kind, source string) openAIClientSessionIdentityResolution {
	return openAIClientSessionIdentityResolution{metadata: openAIClientSessionIdentityMetadata{
		Status: status,
		Kind:   kind,
		Source: source,
	}}
}

func resolvedOpenAIClientSessionIdentity(kind, value, source string) openAIClientSessionIdentityResolution {
	return openAIClientSessionIdentityResolution{
		metadata: openAIClientSessionIdentityMetadata{
			Status: openAIClientSessionIdentityResolved,
			Kind:   kind,
			Source: source,
		},
		identity: openAIClientSessionIdentity{kind: kind, value: value},
	}
}

func identityFailureSource(header openAIIdentityValue, invalidBody bool) string {
	if header.status == openAIClientSessionIdentityInvalid && invalidBody {
		return "header_body"
	}
	if header.status == openAIClientSessionIdentityInvalid {
		return "header"
	}
	return "body"
}

func identityConflictSource(header openAIIdentityValue, body string) string {
	if header.status == openAIClientSessionIdentityConflict {
		return "header"
	}
	if header.value != "" && body != "" {
		return "header_body"
	}
	return "none"
}

func openAIIdentityHeader(c *gin.Context, headers []string) openAIIdentityValue {
	var resolved string
	for _, header := range headers {
		raw := c.GetHeader(header)
		if strings.TrimSpace(raw) == "" {
			continue
		}
		value := sanitizeSessionID(raw)
		if value == "" {
			return openAIIdentityValue{status: openAIClientSessionIdentityInvalid}
		}
		if resolved != "" && resolved != value {
			return openAIIdentityValue{status: openAIClientSessionIdentityConflict}
		}
		resolved = value
	}
	if resolved == "" {
		return openAIIdentityValue{status: openAIClientSessionIdentityMissing}
	}
	return openAIIdentityValue{value: resolved, status: openAIClientSessionIdentityResolved}
}

func openAIClientMetadataIdentity(view gjson.Result, path string) (string, bool) {
	result := view.Get(path)
	if !result.Exists() {
		return "", false
	}
	if result.Type != gjson.String {
		return "", true
	}
	if strings.TrimSpace(result.String()) == "" {
		return "", false
	}
	value := sanitizeSessionID(result.String())
	return value, value == ""
}

func openAIIdentityValuesConflict(headerValue, bodyValue string) bool {
	return headerValue != "" && bodyValue != "" && headerValue != bodyValue
}
