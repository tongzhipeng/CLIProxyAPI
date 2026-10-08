package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func unixString(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10)
}

func quotaResetTestAuth(id string, signals map[string]string, observedAt time.Time) *Auth {
	return &Auth{ID: id, Quota: QuotaState{Signals: signals, ObservedAt: observedAt}}
}

func pickQuotaResetFirst(t *testing.T, provider string, auths []*Auth) *Auth {
	t.Helper()
	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), provider, "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatal("Pick() auth = nil")
	}
	return got
}

func TestQuotaResetFirstSelector_ClaudePrefersSoonestWeeklyReset(t *testing.T) {
	t.Parallel()
	now := time.Now()
	auths := []*Auth{
		quotaResetTestAuth("a", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(5 * 24 * time.Hour)),
			"Anthropic-Ratelimit-Unified-5h-Reset": unixString(now.Add(10 * time.Minute)),
		}, now),
		quotaResetTestAuth("b", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(1 * 24 * time.Hour)),
			"Anthropic-Ratelimit-Unified-5h-Reset": unixString(now.Add(4 * time.Hour)),
		}, now),
	}
	if got := pickQuotaResetFirst(t, "claude", auths); got.ID != "b" {
		t.Fatalf("Pick() auth.ID = %q, want %q (soonest 7d reset)", got.ID, "b")
	}
}

func TestQuotaResetFirstSelector_CodexUsesLongestWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	auths := []*Auth{
		quotaResetTestAuth("a", map[string]string{
			"X-Codex-Primary-Window-Minutes":   "300",
			"X-Codex-Primary-Reset-At":         unixString(now.Add(5 * time.Minute)),
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       unixString(now.Add(6 * 24 * time.Hour)),
		}, now),
		quotaResetTestAuth("b", map[string]string{
			"X-Codex-Primary-Window-Minutes":        "300",
			"X-Codex-Primary-Reset-After-Seconds":   "14000",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": "7200",
		}, now),
	}
	if got := pickQuotaResetFirst(t, "codex", auths); got.ID != "b" {
		t.Fatalf("Pick() auth.ID = %q, want %q (soonest weekly reset)", got.ID, "b")
	}
}

func TestQuotaResetFirstSelector_UnknownOrStaleFallsBackAfterKnown(t *testing.T) {
	t.Parallel()
	now := time.Now()
	auths := []*Auth{
		quotaResetTestAuth("a", nil, time.Time{}),
		quotaResetTestAuth("b", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(-time.Minute)),
		}, now.Add(-7*24*time.Hour)),
		quotaResetTestAuth("c", map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(3 * 24 * time.Hour)),
		}, now),
	}
	if got := pickQuotaResetFirst(t, "claude", auths); got.ID != "c" {
		t.Fatalf("Pick() auth.ID = %q, want %q (only future reset)", got.ID, "c")
	}

	if got := pickQuotaResetFirst(t, "claude", auths[:2]); got.ID != "a" {
		t.Fatalf("Pick() auth.ID = %q, want %q (fill-first fallback)", got.ID, "a")
	}
}

func TestQuotaResetFirstSelector_DevinSignals(t *testing.T) {
	t.Parallel()
	now := time.Now()
	auths := []*Auth{
		quotaResetTestAuth("a", map[string]string{
			"weekly_quota_reset_at": now.Add(4 * 24 * time.Hour).UTC().Format(time.RFC3339),
		}, now),
		quotaResetTestAuth("b", map[string]string{
			"weekly_quota_reset_at": now.Add(2 * 24 * time.Hour).UTC().Format(time.RFC3339),
		}, now),
	}
	if got := pickQuotaResetFirst(t, "devin", auths); got.ID != "b" {
		t.Fatalf("Pick() auth.ID = %q, want %q", got.ID, "b")
	}
}

func TestQuotaResetFirstSelector_SkipsCooldownAuth(t *testing.T) {
	t.Parallel()
	now := time.Now()
	soonest := quotaResetTestAuth("a", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(time.Hour)),
	}, now)
	soonest.Quota.Exceeded = true
	soonest.Quota.Reason = "credential_quota"
	soonest.Quota.NextRecoverAt = now.Add(time.Hour)
	later := quotaResetTestAuth("b", map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(48 * time.Hour)),
	}, now)

	if got := pickQuotaResetFirst(t, "claude", []*Auth{soonest, later}); got.ID != "b" {
		t.Fatalf("Pick() auth.ID = %q, want %q (soonest is cooling down)", got.ID, "b")
	}
}

func TestManagerQuotaResetFirstSelector_UsesObservedSignals(t *testing.T) {
	t.Parallel()
	now := time.Now()
	manager := NewManager(nil, &QuotaResetFirstSelector{}, nil)
	manager.executors["claude"] = schedulerTestExecutor{}
	manager.auths["auth-a"] = &Auth{ID: "auth-a", Provider: "claude", Quota: QuotaState{Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(5 * 24 * time.Hour)),
	}, ObservedAt: now}}
	manager.auths["auth-b"] = &Auth{ID: "auth-b", Provider: "claude", Quota: QuotaState{Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset": unixString(now.Add(2 * time.Hour)),
	}, ObservedAt: now}}

	got, _, errPick := manager.pickNext(context.Background(), "claude", "", cliproxyexecutor.Options{}, map[string]struct{}{})
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if got == nil || got.ID != "auth-b" {
		t.Fatalf("pickNext() auth = %v, want auth-b", got)
	}
}
