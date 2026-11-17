package service

import (
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Start each account attempt with the caller's declarations. A shared gin
// context survives failover and compatibility retries; previous sources must
// not describe the next account's request. Only fixed source flags are kept.
func beginOpenAIResponsesValidation(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	ResetOpenAIResponsesValidationSearchSources(c)
	carriers, _ := openAIRequestToolCategories(body)
	c.Set(openAIClientToolsObservedKey, len(carriers) != 1 || carriers[0] != "none")
	c.Set(openAIFinalProtocolDiagnosticKey, openAIFinalProtocolDiagnostic{})
	root := parseRawJSONView(body)
	if gjsonToolsContainWebSearch(root.Get("tools")) {
		MarkOpenAIResponsesValidationSearchSource(c, OpenAIResponsesValidationSearchClient)
		return
	}
	input := root.Get("input")
	if !input.IsArray() {
		return
	}
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == openAIAdditionalToolsItemType && gjsonToolsContainWebSearch(item.Get("tools")) {
			MarkOpenAIResponsesValidationSearchSource(c, OpenAIResponsesValidationSearchClient)
			return false
		}
		return true
	})
}
