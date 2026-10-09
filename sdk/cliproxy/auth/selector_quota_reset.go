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
	picked := available[0]
	var pickedReset time.Time
	for _, candidate := range available {
		reset, ok := quotaResetForModel(ctx, candidate, model, now)
		if ok && (pickedReset.IsZero() || reset.Before(pickedReset)) {
			picked, pickedReset = candidate, reset
		}
	}
	return picked, nil
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
