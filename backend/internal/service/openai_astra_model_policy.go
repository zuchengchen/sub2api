package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// OpenAIAstraPolicyExtraKey carries the original probe history, including the
// model that was probed. It is projected into scheduler snapshots as well.
const OpenAIAstraPolicyExtraKey = "codex_tibo_verdict:http"

type OpenAIOAuthModelPolicy struct {
	Verdict   string     `json:"verdict"`
	Scope     string     `json:"scope"`
	SampledAt *time.Time `json:"sampled_at,omitempty"`
}

// LatestOpenAIAstraSample reads definitive observations, never the independently
// debounced verdict. Unknown observations and elapsed time do not erase a point.
// Requiring its source and timestamp prevents malformed/foreign history from
// silently granting access. Filtering/sorting only touches a private copy.
func LatestOpenAIAstraSample(raw any) (string, time.Time) {
	samples := validOpenAIAstraSamples(raw, time.Now())
	if len(samples) == 0 {
		return string(openAITiboUnknown), time.Time{}
	}
	last := samples[len(samples)-1]
	return string(last.Verdict), last.At
}

func validOpenAIAstraSamples(raw any, now time.Time) []openAITiboRecentSample {
	model, _ := openAITiboExtraField(raw, "model").(string)
	if !isOpenAIGPT6AstraModel(model) {
		return nil
	}
	var rows []map[string]any
	switch samples := openAITiboExtraField(raw, "recent_samples").(type) {
	case []map[string]any:
		rows = samples
	case []any:
		for _, sample := range samples {
			if row, ok := sample.(map[string]any); ok {
				rows = append(rows, row)
			}
		}
	default:
		encoded, err := json.Marshal(samples)
		if err != nil || json.Unmarshal(encoded, &rows) != nil {
			return nil
		}
	}
	result := make([]openAITiboRecentSample, 0, len(rows))
	for _, row := range rows {
		value, _ := row["verdict"].(string)
		if !openAITiboVerdict(value).definitive() {
			continue
		}
		stamp, _ := row["at"].(string)
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || at.IsZero() || at.After(now) {
			continue
		}
		result = append(result, openAITiboRecentSample{Verdict: openAITiboVerdict(value), At: at})
	}
	slices.SortStableFunc(result, func(a, b openAITiboRecentSample) int { return a.At.Compare(b.At) })
	if len(result) > openAITiboRecentSampleLimit {
		result = result[len(result)-openAITiboRecentSampleLimit:]
	}
	return result
}

func (a *Account) OpenAIOAuthModelPolicy() *OpenAIOAuthModelPolicy {
	if a == nil || !a.IsOpenAIOAuth() {
		return nil
	}
	verdict, at := LatestOpenAIAstraSample(a.Extra[OpenAIAstraPolicyExtraKey])
	policy := &OpenAIOAuthModelPolicy{Verdict: verdict, Scope: "luna_and_images"}
	if verdict == string(openAITiboHealthy) {
		policy.Scope = "non_luna"
	}
	policy.SampledAt = openAITiboTimePtr(at)
	return policy
}

// AllowsOpenAIOAuthModelPolicy is an account-type-specific model gate. It does
// not replace quota, group permissions, retirement or upstream capability gates.
func (a *Account) AllowsOpenAIOAuthModelPolicy(model string) bool {
	policy := a.OpenAIOAuthModelPolicy()
	if policy == nil {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return true // Required-model validation belongs to the request parser.
	}
	if IsGPTImageGenerationModel(lastOpenAIModelSegment(model)) {
		return true
	}
	model = normalizeOpenAIModelForUpstream(a, model)
	luna := isOpenAIGPT6LunaModel(model) || isOpenAIGPT56LunaModel(model)
	if policy.Scope == "non_luna" {
		return !luna && isOpenAIOAuthServableModel(model)
	}
	return luna
}

func validateOpenAIAstraModelPolicy(account *Account, model string) error {
	if account.AllowsOpenAIOAuthModelPolicy(model) {
		return nil
	}
	// No upstream request took place. Let HTTP handlers try another eligible
	// account instead of penalizing this account or rewriting the requested model.
	return &UpstreamFailoverError{
		StatusCode:             503,
		ResponseBody:           []byte(`{"error":{"type":"server_error","code":"model_unavailable","message":"No eligible account for the requested model"}}`),
		RequestScopedTransient: true,
	}
}

// CheckOpenAIAstraModelPolicy rechecks a queued request or connection using
// the shared scheduler cache. Account state is copied, never patched in place.
func (s *OpenAIGatewayService) CheckOpenAIAstraModelPolicy(ctx context.Context, account *Account, model string) error {
	if account == nil || !account.IsOpenAIOAuth() {
		return nil
	}
	latest := account
	if s != nil && s.schedulerSnapshot != nil {
		var err error
		latest, err = s.getSchedulableAccount(ctx, account.ID)
		if err != nil || latest == nil {
			return fmt.Errorf("account model policy is unavailable; reconnect to select an eligible account")
		}
	}
	return validateOpenAIAstraModelPolicy(latest, model)
}

// Catalog filtering must allow a model when another account can still serve it.
func openAIAstraModelUnavailableForAccounts(accounts []Account, model string) bool {
	blocked := false
	for i := range accounts {
		account := &accounts[i]
		if account.IsOpenAIOAuth() && !account.AllowsOpenAIOAuthModelPolicy(model) {
			blocked = true
		} else if account.IsModelSupported(model) {
			return false
		}
	}
	return blocked
}
