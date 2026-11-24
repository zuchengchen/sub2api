package openai

import "github.com/tidwall/gjson"

// CacheCreationTokensFromUsage returns the reported aggregate and any complete
// split across 30-minute and 12-hour cache writes.
func CacheCreationTokensFromUsage(value gjson.Result) (total, ttl30m, ttl12h int) {
	total = cacheCreationTokenCount(value)
	ttl30m, ttl12h = cacheCreationTokensByTTL(value, total)
	return total, ttl30m, ttl12h
}

func cacheCreationTokenCount(value gjson.Result) int {
	for _, nested := range []gjson.Result{
		value.Get("input_tokens_details.cache_write_tokens"),
		value.Get("prompt_tokens_details.cache_write_tokens"),
		value.Get("input_tokens_details.cache_creation_tokens"),
		value.Get("prompt_tokens_details.cache_creation_tokens"),
	} {
		if nested.Exists() {
			return max(int(nested.Int()), 0)
		}
	}

	for _, field := range []string{"cache_write_tokens", "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation_tokens"} {
		if tokens := int(value.Get(field).Int()); tokens > 0 {
			return tokens
		}
	}
	return 0
}

// Only reported usage determines the lifetime. Missing, partial or inconsistent
// details keep the aggregate's legacy price; request TTLs cannot prove writes.
func cacheCreationTokensByTTL(value gjson.Result, aggregate int) (int, int) {
	for _, path := range []string{"input_tokens_details", "prompt_tokens_details"} {
		details := value.Get(path + ".cache_write_tokens_details")
		if !details.Exists() {
			continue
		}
		short, long := details.Get("ttl_30m_tokens"), details.Get("ttl_12h_tokens")
		if short.Type != gjson.Number || long.Type != gjson.Number {
			return 0, 0
		}
		a, b := short.Int(), long.Int()
		if a < 0 || b < 0 || float64(a) != short.Float() || float64(b) != long.Float() ||
			a > int64(aggregate) || b != int64(aggregate)-a {
			return 0, 0
		}
		return int(a), int(b)
	}
	return 0, 0
}
