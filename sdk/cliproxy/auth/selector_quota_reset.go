package auth

import (
	"context"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// QuotaResetFirstSelector prefers the available credential whose longest observed
// quota window resets soonest, so quota that would expire unused is consumed first.
// Antigravity uses the requested model bucket from active quota probes.
// Credentials without a future reset observation keep fill-first order after them.
type QuotaResetFirstSelector struct{}

// Pick selects the available auth with the earliest upcoming quota window reset.
func (s *QuotaResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	best := evaluateCandidateQuota(ctx, available[0], model, 0, now)
	for i := 1; i < len(available); i++ {
		curr := evaluateCandidateQuota(ctx, available[i], model, i, now)
		if isBetterCandidate(curr, best) {
			best = curr
		}
	}
	return best.auth, nil
}

type candidateQuotaInfo struct {
	auth  *Auth
	tier  int // 1 = active with reset, 2 = active unknown, 3 = exhausted with reset, 4 = exhausted unknown
	reset time.Time
	index int
}

func isBetterCandidate(curr, best candidateQuotaInfo) bool {
	if curr.tier != best.tier {
		return curr.tier < best.tier
	}
	if (curr.tier == 1 || curr.tier == 3) && !curr.reset.Equal(best.reset) {
		return curr.reset.Before(best.reset)
	}
	return curr.index < best.index
}

func evaluateCandidateQuota(ctx context.Context, a *Auth, model string, index int, now time.Time) candidateQuotaInfo {
	info := candidateQuotaInfo{auth: a, index: index}
	modelState := findModelStateForCandidate(ctx, a, model)

	exhausted := false
	if modelState != nil && isQuotaExhausted(modelState.Quota, now) {
		exhausted = true
	} else if isQuotaExhausted(a.Quota, now) {
		exhausted = true
	}

	if weeklyReset, hasWeekly := candidateWeeklyResetAt(a, now); hasWeekly {
		info.reset = weeklyReset
		if exhausted {
			info.tier = 3
		} else {
			info.tier = 1
		}
		return info
	}

	if modelState != nil {
		q := modelState.Quota
		if raw := q.Signals["reset_time"]; raw != "" {
			if !q.ObservedAt.IsZero() && now.Sub(q.ObservedAt) <= 10*time.Minute {
				if t, ok := firstQuotaSignalTime(q.Signals, "reset_time"); ok && t.After(now) {
					info.reset = t
					if exhausted {
						info.tier = 3
					} else {
						info.tier = 1
					}
					return info
				}
			}
		}
	}

	if reset, ok := quotaWindowResetAt(a.Quota, now); ok {
		info.reset = reset
		if exhausted {
			info.tier = 3
		} else {
			info.tier = 1
		}
		return info
	}

	if reset, ok := quotaWindowResetFromAuth(a, now); ok {
		info.reset = reset
		if exhausted {
			info.tier = 3
		} else {
			info.tier = 1
		}
		return info
	}

	if exhausted {
		info.tier = 4
	} else {
		info.tier = 2
	}
	return info
}

func isQuotaExhausted(q QuotaState, now time.Time) bool {
	if q.Exceeded && (q.NextRecoverAt.IsZero() || q.NextRecoverAt.After(now)) {
		return true
	}
	if raw, ok := q.Signals["remaining_fraction"]; ok && strings.TrimSpace(raw) != "" {
		if val, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && val <= 0 {
			return true
		}
	}
	return false
}

func findModelStateForCandidate(ctx context.Context, a *Auth, model string) *ModelState {
	if a == nil || len(a.ModelStates) == 0 {
		return nil
	}
	model = canonicalModelKey(rewriteModelForAuth(model, a))
	if ctx != nil {
		if models, ok := ctx.Value(quotaResetModelsKey{}).(map[string]string); ok {
			if m, ok := models[a.ID]; ok && m != "" {
				model = m
			}
		}
	}
	if state := a.ModelStates[model]; state != nil {
		return state
	}
	for _, candidate := range tieredModelCandidates(model) {
		if state := a.ModelStates[candidate]; state != nil {
			return state
		}
	}
	return nil
}

func tieredModelCandidates(model string) []string {
	model = strings.TrimSpace(strings.ToLower(model))
	if model == "" {
		return nil
	}
	var candidates []string
	for _, suffix := range []string{"-high", "-medium", "-low", "-extra-low", "-thinking"} {
		if strings.HasSuffix(model, suffix) {
			base := model[:len(model)-len(suffix)]
			candidates = append(candidates, base+"-tiered", base)
			break
		}
	}
	if strings.HasSuffix(model, "-tiered") {
		base := strings.TrimSuffix(model, "-tiered")
		candidates = append(candidates, base+"-high", base)
	} else if len(candidates) == 0 {
		candidates = append(candidates, model+"-tiered")
	}
	return candidates
}

func candidateWeeklyResetAt(a *Auth, now time.Time) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if reset, ok := firstQuotaSignalTime(a.Quota.Signals, "Anthropic-Ratelimit-Unified-7d-Reset"); ok && reset.After(now) {
		return reset, true
	}
	if reset, ok := codexWeeklyQuotaReset(a.Quota, now); ok {
		return reset, true
	}
	if reset, ok := firstQuotaSignalTime(a.Quota.Signals, "weekly_quota_reset_at"); ok && reset.After(now) {
		return reset, true
	}
	if a.Attributes != nil {
		if raw := a.Attributes["weekly_quota_reset_at"]; raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil && t.After(now) {
				return t, true
			}
		}
	}
	if a.Metadata != nil {
		if raw, ok := a.Metadata["weekly_quota_reset_at"].(string); ok && raw != "" {
			if t, err := time.Parse(time.RFC3339, raw); err == nil && t.After(now) {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

func codexWeeklyQuotaReset(q QuotaState, now time.Time) (time.Time, bool) {
	reset, ok := codexQuotaWindowReset(q)
	if !ok || !reset.After(now) {
		return time.Time{}, false
	}
	minutes, _ := strconv.ParseInt(strings.TrimSpace(q.Signals["X-Codex-Secondary-Window-Minutes"]), 10, 64)
	if minutes >= 1440 {
		return reset, true
	}
	return time.Time{}, false
}

func quotaWindowResetFromAuth(a *Auth, now time.Time) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	for _, key := range []string{"reset_time", "daily_quota_reset_at"} {
		if a.Attributes != nil {
			if raw := a.Attributes[key]; raw != "" {
				if t, err := time.Parse(time.RFC3339, raw); err == nil && t.After(now) {
					return t, true
				}
			}
		}
		if a.Metadata != nil {
			if raw, ok := a.Metadata[key].(string); ok && raw != "" {
				if t, err := time.Parse(time.RFC3339, raw); err == nil && t.After(now) {
					return t, true
				}
			}
		}
	}
	return time.Time{}, false
}

// quotaResetModelsKey carries the manager's credential-aware alias resolution.
type quotaResetModelsKey struct{}

func (m *Manager) withQuotaResetModels(ctx context.Context, selector Selector, model string, auths []*Auth) context.Context {
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		selector = affinity.fallback
	}
	if _, ok := selector.(*QuotaResetFirstSelector); !ok {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	models := make(map[string]string, len(auths))
	for _, a := range auths {
		models[a.ID] = m.selectionModelKeyForAuth(a, model)
	}
	return context.WithValue(ctx, quotaResetModelsKey{}, models)
}

func quotaResetForModel(ctx context.Context, a *Auth, model string, now time.Time) (time.Time, bool) {
	model = canonicalModelKey(rewriteModelForAuth(model, a))
	if ctx != nil {
		if models, ok := ctx.Value(quotaResetModelsKey{}).(map[string]string); ok {
			model = models[a.ID]
		}
	}
	if state := a.ModelStates[model]; state != nil {
		q := state.Quota
		if raw := q.Signals["reset_time"]; raw != "" {
			// Active probes can fail while an old reset remains in the future.
			if q.ObservedAt.IsZero() || now.Sub(q.ObservedAt) > 10*time.Minute {
				return time.Time{}, false
			}
			reset, ok := firstQuotaSignalTime(q.Signals, "reset_time")
			return reset, ok && reset.After(now)
		}
	}
	return quotaWindowResetAt(a.Quota, now)
}

// quotaWindowResetAt reports when the longest observed quota window resets.
// A reset that is not in the future means the snapshot predates the current window.
func quotaWindowResetAt(q QuotaState, now time.Time) (time.Time, bool) {
	reset, ok := firstQuotaSignalTime(q.Signals,
		"Anthropic-Ratelimit-Unified-7d-Reset",
		"Anthropic-Ratelimit-Unified-5h-Reset",
		"Anthropic-Ratelimit-Unified-Reset",
	)
	if !ok {
		reset, ok = codexQuotaWindowReset(q)
	}
	if !ok {
		reset, ok = firstQuotaSignalTime(q.Signals, "weekly_quota_reset_at", "daily_quota_reset_at")
	}
	if !ok || !reset.After(now) {
		return time.Time{}, false
	}
	return reset, true
}

func codexQuotaWindowReset(q QuotaState) (time.Time, bool) {
	var best time.Time
	bestMinutes := int64(-1)
	for _, prefix := range []string{"X-Codex-Secondary-", "X-Codex-Primary-"} {
		reset, ok := firstQuotaSignalTime(q.Signals, prefix+"Reset-At")
		if !ok {
			seconds, errParse := strconv.ParseFloat(strings.TrimSpace(q.Signals[prefix+"Reset-After-Seconds"]), 64)
			if errParse != nil || q.ObservedAt.IsZero() {
				continue
			}
			reset = q.ObservedAt.Add(time.Duration(seconds * float64(time.Second)))
		}
		minutes, _ := strconv.ParseInt(strings.TrimSpace(q.Signals[prefix+"Window-Minutes"]), 10, 64)
		if minutes > bestMinutes {
			best, bestMinutes = reset, minutes
		}
	}
	return best, bestMinutes >= 0
}

// firstQuotaSignalTime parses the first present key as Unix seconds or RFC3339.
func firstQuotaSignalTime(signals map[string]string, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		raw := strings.TrimSpace(signals[key])
		if raw == "" {
			continue
		}
		if sec, errParse := strconv.ParseFloat(raw, 64); errParse == nil && sec > 0 {
			return time.Unix(0, int64(sec*float64(time.Second))), true
		}
		if t, errParse := time.Parse(time.RFC3339, raw); errParse == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
