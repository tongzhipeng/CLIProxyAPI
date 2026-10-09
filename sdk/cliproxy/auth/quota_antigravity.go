package auth

import (
	"strings"
	"time"
)

// ObserveAntigravityQuotas atomically replaces the active per-model quota
// observations without changing cooldowns, credentials, or refresh lifecycle.
// Results from a removed, replaced, disabled, or reauthenticated auth are ignored.
func (m *Manager) ObserveAntigravityQuotas(base *Auth, quotas map[string]QuotaState, observedAt time.Time) bool {
	if m == nil || base == nil {
		return false
	}
	m.mu.Lock()
	current := m.auths[base.ID]
	if current == nil || current.Disabled || current.Status == StatusDisabled ||
		!strings.EqualFold(current.Provider, "antigravity") ||
		current.RegistrationEpoch != base.RegistrationEpoch || CredentialsChanged(base, current) {
		m.mu.Unlock()
		return false
	}
	// A late response must not replace a newer model quota snapshot.
	for _, state := range current.ModelStates {
		if state != nil && state.Quota.Signals["reset_time"] != "" && state.Quota.ObservedAt.After(observedAt) {
			m.mu.Unlock()
			return false
		}
	}
	// Missing buckets in a successful response must not retain an old snapshot.
	for _, state := range current.ModelStates {
		if state != nil && state.Quota.Signals["reset_time"] != "" {
			state.Quota.ClearObservationSignals()
		}
	}
	for model, quota := range quotas {
		if state := ensureModelState(current, model); state != nil {
			state.Quota = mergeQuotaObservation(state.Quota, quota)
		}
	}
	current.Generation++
	snapshot := current.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	return true
}
