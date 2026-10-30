package service

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPerformanceOptimizationGrokSeedEquivalence(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"empty object", `{}`},
		{"model only", `{"model":"grok-4"}`},
		{"chat all system messages", `{"model":"grok-4","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"functions":[{"name":"legacy"}],"instructions":"Be concise.","messages":[{"role":"system","content":"Initial"},{"role":"user","content":"Hello"},{"role":"assistant","content":"Answer"},{"role":"developer","content":[{"text":"Later prompt","type":"text"}]},{"role":"system","content":"Last prompt"}]}`},
		{"responses structured content", `{"model":"grok-4","input":[{"role":"developer","content":{"z":1,"a":2}},{"role":"assistant","content":"ignored"},{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="},{"type":"input_text","text":"hello"}]}]}`},
		{"responses typed input", `{"model":"grok-4","input":[{"type":"input_text","text":"hello"},{"role":"user","content":"later"},{"role":"system","content":"late system"}]}`},
		{"responses input string", `{"model":"grok-4","input":"say \"hello\""}`},
		{"messages before input", `{"messages":[{"role":"user","content":"chat"}],"input":[{"role":"system","content":"must ignore"},{"role":"user","content":"responses"}]}`},
		{"input before messages", `{"input":[{"role":"system","content":"must ignore"},{"role":"user","content":"responses"}],"messages":[{"role":"user","content":"chat"}]}`},
		{"empty messages shadows input", `{"messages":[],"input":[{"role":"system","content":"must ignore"},{"role":"user","content":"must ignore"}]}`},
		{"invalid messages uses input", `{"messages":{},"input":[{"role":"system","content":"system"},{"role":"user","content":"fallback"}]}`},
		{"null messages uses input", `{"messages":null,"input":"fallback"}`},
		{"first empty user is not replaced", `{"messages":[{"role":"user","content":"  "},{"role":"user","content":"later"}]}`},
		{"first empty typed input is not replaced", `{"input":[{"type":"input_text","text":"  "},{"role":"user","content":"later"}]}`},
		{"whitespace roles retain legacy distinction", `{"model":"grok-4","messages":[{"role":" system ","content":"prefix"},{"role":" user ","content":"anchor"}]}`},
		{"whitespace input type", `{"model":"grok-4","input":[{"type":" input_text ","text":"anchor"}]}`},
		{"all empty structured content", `{"tools":[],"functions":[],"instructions":"  ","messages":[{"role":"system","content":{}},{"role":"developer","content":[]},{"role":"user","content":[" ",{"text":""},null,{}]}]}`},
		{"nonempty structured anchor", `{"messages":[{"role":"user","content":[" ",{"text":""},{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}]}]}`},
		{"missing content", `{"messages":[{"role":"system"},{"role":"user"}]}`},
		{"scalar content", `{"messages":[{"role":"system","content":true},{"role":"user","content":0}]}`},
		{"instructions type coercion", `{"tools":{},"functions":false,"instructions":42,"input":"question"}`},
		{"instructions structured canonical JSON", `{"instructions":{"b":2,"a":1},"input":"question"}`},
		{"delimiter escaping", `{"instructions":"foo|system=\"bar\"","input":"hello|first_user=bye"}`},
		{"duplicate fields retain first", `{"tools":[{"name":"first"}],"tools":[{"name":"second"}],"functions":[],"functions":[{"name":"second"}],"instructions":"first","instructions":"second","messages":null,"messages":[{"role":"user","content":"second"}],"input":"first input","input":"second input"}`},
		{"duplicate messages first empty shadows later", `{"messages":[],"messages":[{"role":"system","content":"later"},{"role":"user","content":"later"}],"input":"fallback"}`},
		{"duplicate nested keys retain first", `{"messages":[{"role":"system","role":"developer","content":{"z":1,"z":2,"a":3},"content":"ignored"},{"role":"user","role":"assistant","content":"first","content":"last"}]}`},
		{"escaped keys", `{"instr\u0075ctions":"instruction","mess\u0061ges":[{"ro\u006ce":"system","cont\u0065nt":[{"te\u0078t":"prompt","type":"text"}]},{"role":"user","content":"hello"}]}`},
		{"trailing object", `{"input":"root"}{"messages":[{"role":"system","content":"trailing"},{"role":"user","content":"trailing"}]}`},
		{"trailing quoted fields", `{"input":"root"}"instructions":"trailing"`},
		{"leading garbage", `garbage{"instructions":"prompt","input":"hello"}`},
		{"leading primitive", `null{"instructions":"prompt","input":"hello"}`},
		{"leading number", `123{"instructions":"prompt","input":"hello"}`},
		{"leading quoted text", `"prefix"{"instructions":"prompt","input":"hello"}`},
		{"root array before object", `[{"instructions":"nested","input":"nested"}]{"instructions":"trailing","input":"trailing"}`},
		{"string with braces", `{"metadata":"escaped } [ \" quote","instructions":"prompt","input":"hello"}`},
		{"nested objects", `{"metadata":{"nested":"} ]"},"instructions":"prompt","input":"hello"}`},
		{"truncated string containing brace", `{"instructions":"prompt","input":"still } inside`},
		{"truncated array", `{"instructions":"prompt","messages":[{"role":"user","content":"hello"}`},
		{"invalid UTF8", "{\"instructions\":\"prompt\xff\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\xfe\"}]}"},
		{"missing comma", `{"instructions":"prompt" "input":"hello"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertPerformanceGrokSeedEquivalence(t, []byte(test.body))
		})
	}
}

func TestPerformanceOptimizationGrokSeedEveryTruncation(t *testing.T) {
	for index, body := range []string{
		`{"model":"grok-4","tools":[{"function":{"parameters":{"z":1,"a":2},"name":"lookup"},"type":"function"}],"functions":[{"name":"legacy"}],"instructions":"escaped \" } text","messages":[{"role":"system","content":"System"},{"role":"user","content":[{"type":"text","text":"Hello"}]}],"input":"fallback"}`,
		`{"input":[{"role":"system","content":[{"type":"input_text","text":"prompt"}]},{"type":"input_text","text":"hello"},{"role":"user","content":"later"}],"messages":null,"instructions":"instructions","model":"grok-4"}`,
		`garbage{"instructions":"first","instructions":"second","messages":[{"role":"user","content":"first","content":"second"}],"messages":[],"input":"fallback"}{"instructions":"trailing"}`,
	} {
		t.Run(fmt.Sprintf("body_%d", index), func(t *testing.T) {
			for end := 0; end <= len(body); end++ {
				t.Run(fmt.Sprintf("byte_%d", end), func(t *testing.T) {
					assertPerformanceGrokSeedEquivalence(t, []byte(body[:end]))
				})
			}
		})
	}
}

func assertPerformanceGrokSeedEquivalence(t *testing.T, body []byte) {
	t.Helper()
	original := bytes.Clone(body)
	stable := referencePerformanceOpenAIStablePrefixSessionSeed(body)
	anchor := ""
	if referencePerformanceOpenAIContentSessionUserAnchor(body) {
		anchor = referenceDeriveOpenAIContentSessionSeed(body)
	}
	require.Equal(t, stable, deriveOpenAIStablePrefixSessionSeed(body), "stable prefix bytes")
	require.Equal(t, anchor, deriveOpenAIAnchoredContentSessionSeed(body), "anchor bytes")
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for _, apiKeyID := range []int64{301, 302} {
		c.Set("api_key", &APIKey{ID: apiKeyID, Group: &Group{Platform: PlatformGrok}})
		for _, model := range []string{" GROK-4 ", "grok-4.1"} {
			seed := stable
			if seed == "" {
				seed = anchor
			}
			want := ""
			if seed != "" {
				want = generateSessionUUID(fmt.Sprintf("grok-prompt-cache:v1:%d:%s:%s", apiKeyID, strings.ToLower(strings.TrimSpace(model)), seed))
			}
			require.Equal(t, want, resolveGrokCacheIdentity(c, body, "", model), "isolated identity bytes")
		}
	}
	require.Equal(t, original, body, "seed derivation must not modify the request")
}

func TestPerformanceOptimizationGrokSeedOwnsReturnedBytes(t *testing.T) {
	body := []byte(`{"model":"grok-4","instructions":"Be concise.","tools":[{"function":{"name":"lookup","parameters":{"b":2,"a":1}}}],"messages":[{"role":"system","content":"Prompt"},{"role":"user","content":"Question"}]}`)
	stable := deriveOpenAIStablePrefixSessionSeed(body)
	anchor := deriveOpenAIAnchoredContentSessionSeed(body)
	wantStable := strings.Clone(stable)
	wantAnchor := strings.Clone(anchor)
	for i := range body {
		body[i] = 'x'
	}
	require.Equal(t, wantStable, stable)
	require.Equal(t, wantAnchor, anchor)
}

// Frozen pre-optimization implementations preserve the exact gjson.GetBytes
// behavior for malformed input and duplicate keys, as well as canonical seeds.
func referencePerformanceOpenAIContentSessionUserAnchor(body []byte) bool {
	if len(body) == 0 {
		return false
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		anchored := false
		messages.ForEach(func(_, message gjson.Result) bool {
			if strings.TrimSpace(message.Get("role").String()) != "user" {
				return true
			}
			anchored = hasMeaningfulOpenAIContent(message.Get("content"))
			return false
		})
		return anchored
	}

	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return false
	}
	if input.Type == gjson.String {
		return strings.TrimSpace(input.String()) != ""
	}
	if !input.IsArray() {
		return false
	}

	anchored := false
	input.ForEach(func(_, item gjson.Result) bool {
		if strings.TrimSpace(item.Get("role").String()) == "user" {
			anchored = hasMeaningfulOpenAIContent(item.Get("content"))
			return false
		}
		if strings.TrimSpace(item.Get("type").String()) == "input_text" {
			anchored = strings.TrimSpace(item.Get("text").String()) != ""
			return false
		}
		return true
	})
	return anchored
}

func referencePerformanceOpenAIStablePrefixSessionSeed(body []byte) string {
	if len(body) == 0 {
		return ""
	}

	var b strings.Builder
	hasStablePrefix := false
	appendJSON := func(label string, value gjson.Result) {
		normalized, ok := normalizeNonEmptyCompatSeedJSON(value)
		if !ok {
			return
		}
		_, _ = b.WriteString("|")
		_, _ = b.WriteString(label)
		_, _ = b.WriteString("=")
		_, _ = b.WriteString(normalized)
		hasStablePrefix = true
	}

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		appendJSON("tools", tools)
	}
	if funcs := gjson.GetBytes(body, "functions"); funcs.Exists() && funcs.IsArray() {
		appendJSON("functions", funcs)
	}
	if instructions := gjson.GetBytes(body, "instructions"); strings.TrimSpace(instructions.String()) != "" {
		appendJSON("instructions", instructions)
	}

	appendSystemMessages := func(items gjson.Result) {
		items.ForEach(func(_, item gjson.Result) bool {
			role := strings.TrimSpace(item.Get("role").String())
			switch role {
			case "system", "developer":
				appendJSON(role, item.Get("content"))
			}
			return true
		})
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		appendSystemMessages(messages)
	} else if input := gjson.GetBytes(body, "input"); input.Exists() && input.IsArray() {
		appendSystemMessages(input)
	}

	if !hasStablePrefix {
		return ""
	}
	return contentStablePrefixSessionSeedPrefix + b.String()
}
