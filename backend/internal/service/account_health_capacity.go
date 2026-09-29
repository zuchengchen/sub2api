package service

import "regexp"

// accountHealthCapacityErrorPattern matches request-scoped or pool-wide
// capacity failures. Switching Sub2API accounts that share the same reverse
// proxy cannot fix them, so they must not drive health isolation.
// Lowercase POSIX/RE2-compatible: Go compiles it with (?i) and the account
// health SQL applies it with ~*.
const accountHealthCapacityErrorPattern = `temporarily unavailable|temporarily overloaded|overloaded_error|MODEL_TEMPORARILY_UNAVAILABLE|connection refused|Upstream API request failed`

var accountHealthCapacityErrorRE = regexp.MustCompile(`(?i)` + accountHealthCapacityErrorPattern)
