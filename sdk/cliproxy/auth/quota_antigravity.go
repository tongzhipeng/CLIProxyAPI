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
	var earliestWeekly time.Time
	for model, quota := range quotas {
		if state := ensureModelState(current, model); state != nil {
			state.Quota = mergeQuotaObservation(state.Quota, quota)
		}
		if raw := quota.Signals["weekly_quota_reset_at"]; raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				if earliestWeekly.IsZero() || t.Before(earliestWeekly) {
					earliestWeekly = t
				}
			}
		}
	}
	if !earliestWeekly.IsZero() {
		if current.Quota.Signals == nil {
			current.Quota.Signals = make(map[string]string)
		}
		current.Quota.Signals["weekly_quota_reset_at"] = earliestWeekly.UTC().Format(time.RFC3339Nano)
		current.Quota.ObservedAt = observedAt
		if current.Metadata == nil {
			current.Metadata = make(map[string]any)
		}
		current.Metadata["weekly_quota_reset_at"] = earliestWeekly.UTC().Format(time.RFC3339Nano)
	}
	current.Generation++
	snapshot := current.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	return true
}
