package service

import (
	"crypto/sha256"
	"strconv"
	"strings"
	"sync"
	"time"
)

const contentModerationYuFengHealthTTL = 15 * time.Minute

type contentModerationYuFengEndpointHealthState struct {
	mu           sync.Mutex
	configDigest [sha256.Size]byte
	healthyUntil time.Time
}

func contentModerationYuFengEndpointDigest(endpoint ContentModerationEndpoint) [sha256.Size]byte {
	tokenDigest := sha256.Sum256([]byte(endpoint.Token))
	canonical := strings.Join([]string{
		strings.TrimSpace(endpoint.ID),
		strings.TrimSpace(endpoint.BaseURL),
		strings.TrimSpace(endpoint.Model),
		normalizeContentModerationModelProfile(endpoint.Profile),
		strings.TrimSpace(endpoint.PromptVersion),
		strconv.Itoa(endpoint.TimeoutMS),
		strconv.Itoa(endpoint.InputLimit),
		string(tokenDigest[:]),
	}, "\x00")
	return sha256.Sum256([]byte(canonical))
}

func (s *ContentModerationService) yuFengEndpointHealthState(endpoint ContentModerationEndpoint) *contentModerationYuFengEndpointHealthState {
	digest := contentModerationYuFengEndpointDigest(endpoint)
	candidate := &contentModerationYuFengEndpointHealthState{configDigest: digest}
	actual, _ := s.yuFengEndpointStates.LoadOrStore(endpoint.ID, candidate)
	state, ok := actual.(*contentModerationYuFengEndpointHealthState)
	if !ok || state == nil {
		state = candidate
		s.yuFengEndpointStates.Store(endpoint.ID, state)
	}
	state.mu.Lock()
	if state.configDigest != digest {
		state.configDigest = digest
		state.healthyUntil = time.Time{}
	}
	state.mu.Unlock()
	return state
}

func (s *ContentModerationService) markYuFengEndpointHealthy(endpoint ContentModerationEndpoint, now time.Time) {
	if s == nil {
		return
	}
	state := s.yuFengEndpointHealthState(endpoint)
	state.mu.Lock()
	state.healthyUntil = now.Add(contentModerationYuFengHealthTTL)
	state.mu.Unlock()
}

func (s *ContentModerationService) hasHealthyYuFengEndpoint(cfg *ContentModerationConfig, now time.Time) bool {
	if s == nil || cfg == nil || !cfg.YuFengEnabled {
		return false
	}
	for _, endpoint := range cfg.enabledYuFengSecondLayerEndpoints() {
		state := s.yuFengEndpointHealthState(endpoint)
		state.mu.Lock()
		healthy := now.Before(state.healthyUntil)
		state.mu.Unlock()
		if healthy {
			return true
		}
	}
	return false
}
