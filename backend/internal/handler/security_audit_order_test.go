package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type contentModerationOrderCase struct {
	file       string
	function   string
	auditToken string
}

func TestContentModerationGatePrecedesAccountBillingAndUpstreamSideEffects(t *testing.T) {
	tests := []contentModerationOrderCase{
		{file: "gateway_handler.go", function: "Messages", auditToken: "checkContentModeration"},
		{file: "gateway_handler_chat_completions.go", function: "ChatCompletions", auditToken: "checkContentModeration"},
		{file: "gateway_handler_responses.go", function: "Responses", auditToken: "checkContentModeration"},
		{file: "gemini_v1beta_handler.go", function: "GeminiV1BetaModels", auditToken: "checkContentModeration"},
		{file: "openai_gateway_handler.go", function: "Responses", auditToken: "checkContentModeration"},
		{file: "openai_gateway_count_tokens.go", function: "ResponsesInputTokens", auditToken: "checkContentModeration"},
		{file: "openai_gateway_handler.go", function: "Messages", auditToken: "checkContentModeration"},
		{file: "openai_chat_completions.go", function: "ChatCompletions", auditToken: "checkContentModeration"},
		{file: "openai_images.go", function: "Images", auditToken: "checkContentModeration"},
		{file: "grok_media.go", function: "handleGrokMedia", auditToken: "checkContentModeration"},
		{file: "openai_embeddings.go", function: "Embeddings", auditToken: "checkContentModeration"},
		{file: "openai_alpha_search.go", function: "AlphaSearch", auditToken: "checkContentModeration"},
		{file: "image_task_handler.go", function: "Submit", auditToken: "checkContentModerationBeforeSubmit"},
	}
	sideEffectTokens := []string{
		"CheckBillingEligibility(", "SelectAccount", ".Forward", "acquireResponsesUserSlot(",
		"AcquireUserSlot", "TryAcquireUserSlot", "acquireImageGenerationSlot(",
		"h.tasks.Create(", "h.service.Submit(",
	}
	for _, tt := range tests {
		t.Run(tt.file+"/"+tt.function, func(t *testing.T) {
			functionSource := stripGoComments(goFunctionSource(t, tt.file, tt.function))
			auditIndex := strings.Index(functionSource, tt.auditToken)
			require.NotEqual(t, -1, auditIndex, "missing Content Moderation gate")
			foundSideEffect := false
			for _, sideEffect := range sideEffectTokens {
				index := strings.Index(functionSource, sideEffect)
				if index < 0 {
					continue
				}
				foundSideEffect = true
				require.Lessf(t, auditIndex, index, "%s must run before %s", tt.auditToken, sideEffect)
			}
			require.True(t, foundSideEffect, "coverage case must contain a downstream side effect")
		})
	}
}

func stripGoComments(source string) string {
	source = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(source, "")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(source, "")
}

func goFunctionSource(t *testing.T, filename, functionName string) string {
	t.Helper()
	raw, err := os.ReadFile(filename)
	require.NoError(t, err)
	files := token.NewFileSet()
	parsed, err := parser.ParseFile(files, filename, raw, 0)
	require.NoError(t, err)
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != functionName || function.Body == nil {
			continue
		}
		start := files.Position(function.Pos()).Offset
		end := files.Position(function.End()).Offset
		require.Greater(t, end, start)
		return string(raw[start:end])
	}
	t.Fatalf("function %s not found in %s", functionName, filename)
	return ""
}
