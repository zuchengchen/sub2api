package service

const contentModerationYuFengContextReviewParserStatus = "context_review_pc"

// API clients can submit tool-shaped content directly, so context class alone
// is never authority to downgrade a model risk decision. Keep a visible marker
// for complete non-user pc results while preserving the original block.
func annotateContentModerationYuFengResult(result contentModerationSecondLayerResult, input contentModerationSecondLayerInput) contentModerationSecondLayerResult {
	if !result.Blocked || result.Label != "pc" || input.Evidence.Truncated ||
		!isContentModerationYuFengNonUserContext(input.Fragment.ContextClass) {
		return result
	}
	result.ParserStatus = contentModerationYuFengContextReviewParserStatus
	return result
}

func isContentModerationYuFengNonUserContext(contextClass string) bool {
	switch contextClass {
	case ContentModerationContextAssistant, ContentModerationContextTool, ContentModerationContextServiceLog,
		ContentModerationContextCode, ContentModerationContextConfig:
		return true
	default:
		return false
	}
}
