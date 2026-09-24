package service

import (
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// OpenAICookieWSSlotStatus contains only safe operational metadata. Persisted
// HTTP/WS flags are not evidence that a connection exists in this process.
type OpenAICookieWSSlotStatus struct {
	Slot        int                               `json:"slot"`
	State       string                            `json:"state"`
	CookieReady bool                              `json:"cookie_ready"`
	VerifiedWS  int                               `json:"verified_ws"`
	CapturedAt  *time.Time                        `json:"captured_at,omitempty"`
	RefreshAt   *time.Time                        `json:"refresh_at,omitempty"`
	ExpiresAt   *time.Time                        `json:"expires_at,omitempty"`
	Refresh     *OpenAICookieWSRecoveryDiagnostic `json:"refresh,omitempty"`
	Warmup      *OpenAICookieWSRecoveryDiagnostic `json:"warmup,omitempty"`
}

// OpenAICodexTicketStatuses enriches the persisted legacy status with the
// current gateway's Cookie state. This is a read-only snapshot: fetching an
// account in the admin UI never triggers probes or connection replenishment.
func (s *AccountTestService) OpenAICodexTicketStatuses(account *Account, cfg config.OpenAICodexTicketConfig, now time.Time) []OpenAICodexTicketStatus {
	statuses := OpenAICodexTicketStatuses(account, cfg, now)
	var gateway *OpenAIGatewayService
	if s != nil {
		gateway = s.openaiGatewayService
	}
	for i := range statuses {
		if statuses[i].Mode == openAICookieWSMode {
			statuses[i] = gateway.openAICookieWSRuntimeStatus(account, statuses[i].Model, now)
		}
	}
	return statuses
}

func (s *OpenAIGatewayService) openAICookieWSRuntimeStatus(account *Account, model string, now time.Time) OpenAICodexTicketStatus {
	status := OpenAICodexTicketStatus{
		Model: model, Mode: openAICookieWSMode, Blocked: true,
		CookieGroupsTotal: openAICookieWSSlotCount, WSPerGroup: 1,
		MinimumWS: openAICookieWSMinimumConnections, RecoveryState: "unavailable",
	}
	if account == nil {
		return status
	}
	status.SkipReason = openAICookieWSAccountSkipReason(account, now)
	counts := [openAICookieWSSlotCount]int{}
	if s != nil {
		status.RecoveryState = "recovering"
		counts = s.getOpenAIWSConnPool().CookieVerifiedCounts(account.ID)
		if status.SkipReason == "" && s.isOpenAIAccountRuntimeBlocked(account) {
			status.SkipReason = "runtime_blocked"
		}
		if status.SkipReason == "" && s.getOpenAIAccountModelTransientState().isBlocked(account.ID, openAIAccountModelTransientModel(model), now) {
			status.SkipReason = "model_temporarily_unschedulable"
		}
	}
	var earliest *openAICookieWSTicket
	for slot := 0; slot < openAICookieWSSlotCount; slot++ {
		item := OpenAICookieWSSlotStatus{Slot: slot, State: "unavailable"}
		var ticket *openAICookieWSTicket
		if s == nil {
			ticket = parseOpenAICookieWSTicket(account.ID, model, account.Extra[openAICookieWSExtraKeySlot(model, slot)])
		} else {
			ticket = s.lookupOpenAICookieWSTicketSlot(account, model, slot)
			item.State = "waiting"
			item.Refresh = s.openAICookieWSRecoverySnapshot(account.ID, slot, "refresh")
			item.Warmup = s.openAICookieWSRecoverySnapshot(account.ID, slot, "warmup")
		}
		if ticket != nil && ticket.Slot == slot {
			captured, refresh, expires := ticket.CapturedAt, ticket.RefreshAt, ticket.ExpiresAt
			item.CapturedAt, item.RefreshAt, item.ExpiresAt = &captured, &refresh, &expires
			if ticket.valid(now) {
				status.CookieGroupsValid++
				if earliest == nil || ticket.ExpiresAt.Before(earliest.ExpiresAt) {
					earliest = ticket
				}
			}
			item.CookieReady = ticket.ready(now)
			if item.CookieReady {
				status.CookieGroupsReady++
				item.VerifiedWS = counts[slot]
				status.VerifiedWS += item.VerifiedWS
			}
		}
		if item.VerifiedWS > 0 {
			item.State = "ready"
			// A business request can restore this slot before the next warmup
			// pass. Never present that old warmup failure as a pending retry.
			// Refresh diagnostics remain relevant while a usable Cookie ages.
			if item.Warmup != nil {
				item.Warmup.Phase = "idle"
				item.Warmup.NextAttemptAt = nil
				item.Warmup.LastError = nil
			}
		} else if s != nil {
			// Prefer an active operation, then the operation needed to restore
			// this slot. Old refresh errors must not obscure a warmup failure.
			for _, diagnostic := range []*OpenAICookieWSRecoveryDiagnostic{item.Refresh, item.Warmup} {
				if diagnostic != nil && cookieWSRecoveryPhaseActive(diagnostic.Phase) {
					item.State = diagnostic.Phase
					break
				}
			}
			if item.State == "waiting" {
				diagnostic := item.Refresh
				if item.CookieReady {
					diagnostic = item.Warmup
				}
				if diagnostic != nil && diagnostic.NextAttemptAt != nil && now.Before(*diagnostic.NextAttemptAt) {
					item.State = "backoff"
				}
			}
		}
		if status.SkipReason != "" {
			item.State = "paused"
		}
		status.CookieSlots = append(status.CookieSlots, item)
	}
	if earliest != nil {
		captured, refresh, expires := earliest.CapturedAt, earliest.RefreshAt, earliest.ExpiresAt
		status.CapturedAt, status.RefreshAt, status.ExpiresAt = &captured, &refresh, &expires
		status.RemainingSeconds = int64(expires.Sub(now) / time.Second)
		if status.RemainingSeconds < 0 {
			status.RemainingSeconds = 0
		}
	}
	status.Ready = status.SkipReason == "" && status.VerifiedWS > 0
	status.Blocked = !status.Ready
	switch {
	case status.SkipReason != "":
		status.RecoveryState = "paused"
	case status.VerifiedWS >= status.MinimumWS:
		status.RecoveryState = "ready"
	case status.VerifiedWS > 0:
		status.RecoveryState = "partial"
	}
	return status
}

func cookieWSRecoveryPhaseActive(phase string) bool {
	switch phase {
	case "restoring", "harvesting", "validating", "warming":
		return true
	default:
		return false
	}
}
