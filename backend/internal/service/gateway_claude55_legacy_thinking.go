package service

import (
	"fmt"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeAndValidateClaude55Request adapts a native Messages request only
// after its actual account model mapping is known. Refresh derived effort and
// thinking fields together with the body so forwarding and billing agree.
func normalizeAndValidateClaude55Request(parsed *ParsedRequest, model string) error {
	body, changed, err := normalizeClaude55LegacyThinkingBody(parsed.Body.Bytes(), model)
	if err != nil {
		return err
	}
	if err := validateClaude55Request(body, model); err != nil {
		return err
	}
	if changed {
		return parsed.ReplaceBody(body)
	}
	return nil
}

// Older Claude clients send enabled+budget_tokens even for adaptive-only
// models. Translate that enabled intent while preserving an explicit effort.
// Budget-derived effort is a compatibility heuristic, not an exact token cap.
// Disabled/between_tools modes and other model families keep their contracts.
func normalizeClaude55LegacyThinkingBody(body []byte, model string) ([]byte, bool, error) {
	if !isClaude55SignedThinkingModel(model) || gjson.GetBytes(body, "thinking.type").String() != "enabled" {
		return body, false, nil
	}
	budget := gjson.GetBytes(body, "thinking.budget_tokens")
	var budgetTokens int64
	if budget.Exists() {
		var err error
		budgetTokens, err = strconv.ParseInt(budget.Raw, 10, 64)
		if budget.Type != gjson.Number || err != nil || budgetTokens < 1024 {
			return nil, false, fmt.Errorf("%s legacy thinking.budget_tokens must be an integer >= 1024; use thinking.type=adaptive and output_config.effort instead", model)
		}
	}
	effortValue := gjson.GetBytes(body, "output_config.effort")
	effort := effortValue.String()
	if effortValue.Exists() {
		valid := false
		if effortValue.Type == gjson.String {
			for _, level := range claude.EffortLevelsForModel(model) {
				if effort == level {
					valid = true
					break
				}
			}
		}
		if !valid {
			return nil, false, fmt.Errorf("%s output_config.effort must be low, medium, high, xhigh or max", model)
		}
	} else {
		if !budget.Exists() {
			return nil, false, fmt.Errorf("%s legacy thinking.type=enabled requires budget_tokens or output_config.effort; use thinking.type=adaptive instead", model)
		}
		effort = claude55LegacyBudgetEffort(budgetTokens)
	}
	if output := gjson.GetBytes(body, "output_config"); output.Exists() && !output.IsObject() {
		return nil, false, fmt.Errorf("%s output_config must be an object", model)
	}
	updated, err := sjson.SetBytes(body, "thinking.type", "adaptive")
	if err != nil {
		return nil, false, fmt.Errorf("normalize legacy thinking type: %w", err)
	}
	updated, err = sjson.DeleteBytes(updated, "thinking.budget_tokens")
	if err != nil {
		return nil, false, fmt.Errorf("normalize legacy thinking budget: %w", err)
	}
	if !effortValue.Exists() {
		updated, err = sjson.SetBytes(updated, "output_config.effort", effort)
		if err != nil {
			return nil, false, fmt.Errorf("normalize legacy thinking effort: %w", err)
		}
	}
	return updated, true, nil
}

func claude55LegacyBudgetEffort(budget int64) string {
	// Match the existing bridge's low/medium/high/max budget anchors, using
	// xhigh for the interval between high (10,240) and max (32,768).
	switch {
	case budget <= 1024:
		return "low"
	case budget <= 4096:
		return "medium"
	case budget <= 10240:
		return "high"
	case budget < 32768:
		return "xhigh"
	default:
		return "max"
	}
}
