package repository

import (
	"math"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

// accountTempUnschedAuditComponent is the ops_system_logs component for every
// persisted temp-unschedulable transition. The "audit" substring makes the
// Ops system-log sink index these info-level events even when the global log
// level is higher (production runs at "error"), so a later "why was this
// account paused" question can be answered from ops_system_logs.
const accountTempUnschedAuditComponent = "audit.account_temp_unschedulable"

const (
	accountTempUnschedAuditActionSet     = "set"
	accountTempUnschedAuditActionCleared = "cleared"

	// accountTempUnschedAuditReasonMaxRunes bounds the reason copied into the
	// log row; the full reason stays on the account row itself.
	accountTempUnschedAuditReasonMaxRunes = 512
	// accountTempUnschedAuditCallerDepth is how many frames outside this
	// package are recorded. Wrappers such as RateLimitService.ClearRateLimit
	// need at least one more frame to show which subsystem triggered them.
	accountTempUnschedAuditCallerDepth = 3
)

var (
	repositoryPkgPath   = reflect.TypeOf(accountRepository{}).PkgPath()
	internalPkgPrefix   = strings.TrimSuffix(repositoryPkgPath, "repository")
	repositoryFuncStart = repositoryPkgPath + "."
)

// recordAccountTempUnschedSet records a temp-unschedulable write that was
// actually applied. Call it only after the UPDATE affected the row: every
// writer in this repository only applies when it moves the deadline forward,
// so until is the effective deadline.
func recordAccountTempUnschedSet(writer string, accountID int64, until time.Time, reason string) {
	fields := map[string]any{
		"account_id":       accountID,
		"action":           accountTempUnschedAuditActionSet,
		"writer":           writer,
		"until":            until.UTC().Format(time.RFC3339),
		"duration_seconds": int64(math.Round(time.Until(until).Seconds())),
		"reason":           accountTempUnschedAuditReason(reason),
		"caller":           accountTempUnschedAuditCaller(),
	}
	logger.WriteSinkEvent("info", accountTempUnschedAuditComponent, "account temp-unschedulable set", fields)
}

// recordAccountTempUnschedCleared records that an active pause was lifted
// before it expired. previousUntil is the deadline read under the same row
// lock as the clear; callers must not invoke this for rows whose pause was
// already absent or expired (ClearRateLimit clears unconditionally).
func recordAccountTempUnschedCleared(writer string, accountID int64, previousUntil time.Time) {
	fields := map[string]any{
		"account_id":               accountID,
		"action":                   accountTempUnschedAuditActionCleared,
		"writer":                   writer,
		"previous_until":           previousUntil.UTC().Format(time.RFC3339),
		"remaining_before_seconds": int64(math.Round(time.Until(previousUntil).Seconds())),
		"caller":                   accountTempUnschedAuditCaller(),
	}
	logger.WriteSinkEvent("info", accountTempUnschedAuditComponent, "account temp-unschedulable cleared", fields)
}

func accountTempUnschedAuditReason(reason string) string {
	reason = logredact.RedactText(reason)
	runes := []rune(reason)
	if len(runes) > accountTempUnschedAuditReasonMaxRunes {
		return string(runes[:accountTempUnschedAuditReasonMaxRunes]) + "…"
	}
	return reason
}

// accountTempUnschedAuditCaller returns the nearest frames outside this
// package's production code, e.g.
// "service.(*RateLimitService).ClearRateLimit <- service.(*RateLimitService).RecoverAccountState".
// Only runs on applied writes, which are rare, so the stack walk is cheap.
func accountTempUnschedAuditCaller() string {
	pcs := make([]uintptr, 24)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	chain := make([]string, 0, accountTempUnschedAuditCallerDepth)
	for len(chain) < accountTempUnschedAuditCallerDepth {
		frame, more := frames.Next()
		if frame.Function != "" && !skipAccountTempUnschedAuditFrame(frame) {
			chain = append(chain, strings.TrimPrefix(frame.Function, internalPkgPrefix))
		}
		if !more {
			break
		}
	}
	return strings.Join(chain, " <- ")
}

func skipAccountTempUnschedAuditFrame(frame runtime.Frame) bool {
	if strings.HasPrefix(frame.Function, "runtime.") {
		return true
	}
	// Skip this package's production frames (the writers themselves) but keep
	// tests, so a test caller is reported like any external caller.
	return strings.HasPrefix(frame.Function, repositoryFuncStart) && !strings.HasSuffix(frame.File, "_test.go")
}
