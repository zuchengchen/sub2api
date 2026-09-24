package config

import (
	"fmt"
	"strings"
)

func validateOpenAICodexTicketMode(cfg OpenAICodexTicketConfig) error {
	switch strings.ToLower(strings.TrimSpace(cfg.Mode)) {
	case "", "turn_state", "cookie_ws":
	default:
		return fmt.Errorf("gateway.openai_codex_ticket.mode must be turn_state or cookie_ws")
	}
	for _, id := range cfg.CookieWSAccountIDs {
		if id <= 0 {
			return fmt.Errorf("gateway.openai_codex_ticket.cookie_ws_account_ids must contain positive account IDs")
		}
	}
	return nil
}
