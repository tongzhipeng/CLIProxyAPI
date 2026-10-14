package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"

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

func TestQuotaResetFirstSelector_AntigravityModelScope(t *testing.T) {
	now := time.Now()
	a := quotaResetTestAuth("a", nil, now)
	b := quotaResetTestAuth("b", nil, now)
	a.ModelStates = map[string]*ModelState{
		"gemini-test": {Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{"reset_time": now.Add(4 * time.Hour).Format(time.RFC3339)}}},
		"claude-test": {Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{"reset_time": now.Add(time.Hour).Format(time.RFC3339)}}},
	}
	b.ModelStates = map[string]*ModelState{
		"gemini-test": {Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{"reset_time": now.Add(2 * time.Hour).Format(time.RFC3339)}}},
		"claude-test": {Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{"reset_time": now.Add(3 * time.Hour).Format(time.RFC3339)}}},
	}
	for _, tc := range []struct{ model, want string }{{"gemini-test", "b"}, {"claude-test", "a"}, {"unknown", "a"}} {
		got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", tc.model, cliproxyexecutor.Options{}, []*Auth{b, a})
		if err != nil || got.ID != tc.want {
			t.Fatalf("Pick(%s) = %v, %v; want %s", tc.model, got, err, tc.want)
		}
	}
	b.ModelStates["gemini-test"].Quota.ObservedAt = now.Add(-11 * time.Minute)
	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-test", cliproxyexecutor.Options{}, []*Auth{a, b})
	if err != nil || got.ID != "a" {
		t.Fatalf("stale quota selected: %v, %v", got, err)
	}
}

func TestQuotaResetFirstSelector_AntigravityManagerAlias(t *testing.T) {
	for _, affinity := range []bool{false, true} {
		for _, mixed := range []bool{false, true} {
			t.Run(strconv.FormatBool(affinity)+"/mixed="+strconv.FormatBool(mixed), func(t *testing.T) {
				var selector Selector = &QuotaResetFirstSelector{}
				if affinity {
					selector = NewSessionAffinitySelector(selector)
					defer selector.(*SessionAffinitySelector).Stop()
				}
				manager := NewManager(nil, selector, nil)
				manager.RegisterExecutor(&noForkAliasTestExecutor{id: "antigravity"})
				manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{"antigravity": {{Name: "gemini-test", Alias: "friendly", Fork: false}}})
				now := time.Now()
				for i, id := range []string{"quota-alias-a", "quota-alias-b"} {
					a := &Auth{ID: id, Provider: "antigravity", Prefix: "team", Status: StatusActive, ModelStates: map[string]*ModelState{
						"gemini-test": {Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{"reset_time": now.Add(time.Duration(4-i*2) * time.Hour).Format(time.RFC3339)}}},
					}}
					if _, err := manager.Register(context.Background(), a); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(id, "antigravity", []*registry.ModelInfo{{ID: "team/friendly"}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				}
				var got *Auth
				var err error
				if mixed {
					got, _, _, err = manager.pickNextMixed(context.Background(), []string{"antigravity"}, "team/friendly(1024)", cliproxyexecutor.Options{}, map[string]struct{}{})
				} else {
					got, _, err = manager.pickNext(context.Background(), "antigravity", "team/friendly(1024)", cliproxyexecutor.Options{}, map[string]struct{}{})
				}
				if err != nil || got == nil || got.ID != "quota-alias-b" {
					t.Fatalf("alias pick = %v, %v; want quota-alias-b", got, err)
				}
			})
		}
	}
}

func TestAntigravityQuotaObservationSurvivesRequest(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	now := time.Now()
	a := &Auth{ID: "antigravity-observation", Provider: "antigravity", ModelStates: map[string]*ModelState{
		"gemini-test": {Status: StatusActive, Quota: QuotaState{ObservedAt: now, Signals: map[string]string{"reset_time": now.Add(time.Hour).Format(time.RFC3339)}}},
	}}
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	manager.MarkResult(context.Background(), Result{AuthID: a.ID, Provider: "antigravity", Model: "gemini-test", Success: true})
	got, _ := manager.GetByID(a.ID)
	if got.ModelStates["gemini-test"].Quota.Signals["reset_time"] == "" {
		t.Fatal("request erased active quota observation")
	}
}

func TestQuotaResetFirstSelector_AntigravityTieredMapping(t *testing.T) {
	now := time.Now()
	george := quotaResetTestAuth("george", nil, now)
	kevin := quotaResetTestAuth("kevin", nil, now)
	george.Provider = "antigravity"
	kevin.Provider = "antigravity"
	george.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":         now.Add(3 * time.Hour).Format(time.RFC3339),
					"remaining_fraction": "0.8",
				},
			},
		},
	}
	kevin.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":         now.Add(1 * time.Hour).Format(time.RFC3339),
					"remaining_fraction": "0.8",
				},
			},
		},
	}

	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{george, kevin})
	if err != nil || got == nil || got.ID != "kevin" {
		t.Fatalf("Pick(gemini-3.8-flash-high) = %v, %v; want kevin (soonest reset in tiered bucket)", got, err)
	}
}

func TestQuotaResetFirstSelector_AntigravityExhaustedQuotaSkipped(t *testing.T) {
	now := time.Now()
	george := quotaResetTestAuth("george", nil, now)
	kevin := quotaResetTestAuth("kevin", nil, now)
	george.Provider = "antigravity"
	kevin.Provider = "antigravity"
	// George's reset is sooner (1h), but quota is exhausted (remaining_fraction = 0)
	george.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":         now.Add(1 * time.Hour).Format(time.RFC3339),
					"remaining_fraction": "0",
				},
			},
		},
	}
	// Kevin's reset is later (3h), but has quota remaining (remaining_fraction = 0.95)
	kevin.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":         now.Add(3 * time.Hour).Format(time.RFC3339),
					"remaining_fraction": "0.95",
				},
			},
		},
	}

	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{george, kevin})
	if err != nil || got == nil || got.ID != "kevin" {
		t.Fatalf("Pick(gemini-3.8-flash-high) = %v, %v; want kevin (george quota is exhausted)", got, err)
	}
}

func TestQuotaResetFirstSelector_AntigravityCredentialWeeklyResetFallback(t *testing.T) {
	now := time.Now()
	george := quotaResetTestAuth("george", nil, now)
	kevin := quotaResetTestAuth("kevin", nil, now)
	george.Provider = "antigravity"
	kevin.Provider = "antigravity"
	// George's weekly reset is 3 days later, Kevin's weekly reset is in 12 hours
	george.Quota.Signals = map[string]string{
		"weekly_quota_reset_at": now.Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}
	george.Quota.ObservedAt = now
	kevin.Quota.Signals = map[string]string{
		"weekly_quota_reset_at": now.Add(12 * time.Hour).UTC().Format(time.RFC3339),
	}
	kevin.Quota.ObservedAt = now

	// When model has no specific model bucket reset, pick by credential-level weekly reset
	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "some-unbucketed-model", cliproxyexecutor.Options{}, []*Auth{george, kevin})
	if err != nil || got == nil || got.ID != "kevin" {
		t.Fatalf("Pick(unbucketed) = %v, %v; want kevin (12h vs 3d weekly reset)", got, err)
	}
}

func TestQuotaResetFirstSelector_AllExhaustedRecoversEarliest(t *testing.T) {
	now := time.Now()
	george := quotaResetTestAuth("george", nil, now)
	kevin := quotaResetTestAuth("kevin", nil, now)
	george.Provider = "antigravity"
	kevin.Provider = "antigravity"
	// Both are exhausted, but George resets in 1h and Kevin in 3h
	george.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":         now.Add(1 * time.Hour).Format(time.RFC3339),
					"remaining_fraction": "0",
				},
			},
		},
	}
	kevin.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":         now.Add(3 * time.Hour).Format(time.RFC3339),
					"remaining_fraction": "0",
				},
			},
		},
	}

	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{george, kevin})
	if err != nil || got == nil || got.ID != "george" {
		t.Fatalf("Pick(all exhausted) = %v, %v; want george (earliest recovery)", got, err)
	}
}

func TestQuotaResetFirstSelector_ActiveUnknownBeatsExhausted(t *testing.T) {
	now := time.Now()
	george := quotaResetTestAuth("george", nil, now)
	kevin := quotaResetTestAuth("kevin", nil, now)
	george.Provider = "antigravity"
	kevin.Provider = "antigravity"
	// George is exhausted with a 1h reset
	george.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":         now.Add(1 * time.Hour).Format(time.RFC3339),
					"remaining_fraction": "0",
				},
			},
		},
	}
	// Kevin has no quota signals (active unknown)
	kevin.ModelStates = nil

	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{george, kevin})
	if err != nil || got == nil || got.ID != "kevin" {
		t.Fatalf("Pick(unknown vs exhausted) = %v, %v; want kevin", got, err)
	}
}

func TestQuotaResetFirstSelector_MetadataWeeklyReset(t *testing.T) {
	now := time.Now()
	george := quotaResetTestAuth("george", nil, now)
	kevin := quotaResetTestAuth("kevin", nil, now)
	george.Metadata = map[string]any{
		"weekly_quota_reset_at": now.Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}
	kevin.Metadata = map[string]any{
		"weekly_quota_reset_at": now.Add(12 * time.Hour).UTC().Format(time.RFC3339),
	}

	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{george, kevin})
	if err != nil || got == nil || got.ID != "kevin" {
		t.Fatalf("Pick(metadata weekly) = %v, %v; want kevin", got, err)
	}
}

func TestQuotaResetFirstSelector_RefreshableAccountPlacedLast_PrefersSecondExpiring(t *testing.T) {
	now := time.Now()
	// rowan was the first to expire, but its weekly reset just passed (5 minutes ago).
	// It is now refreshable / entered a new cycle, so it must be placed last.
	rowan := quotaResetTestAuth("rowan", nil, now)
	rowan.Provider = "antigravity"
	rowan.Metadata = map[string]any{
		"weekly_quota_reset_at": now.Add(-5 * time.Minute).UTC().Format(time.RFC3339),
	}
	// kevin is the second expiring account in cache, with 2 hours left before reset.
	kevin := quotaResetTestAuth("kevin", nil, now)
	kevin.Provider = "antigravity"
	kevin.Metadata = map[string]any{
		"weekly_quota_reset_at": now.Add(2 * time.Hour).UTC().Format(time.RFC3339),
	}
	// george is the third expiring account in cache, with 24 hours left.
	george := quotaResetTestAuth("george", nil, now)
	george.Provider = "antigravity"
	george.Metadata = map[string]any{
		"weekly_quota_reset_at": now.Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}

	// Pick should select kevin (the second expiring in cache) because rowan is refreshable and placed last.
	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{rowan, kevin, george})
	if err != nil || got == nil || got.ID != "kevin" {
		t.Fatalf("Pick = %v, %v; want kevin (second expiring, since rowan is refreshable)", got, err)
	}
}

func TestQuotaResetFirstSelector_RefreshableAccountPlacedAfterActiveUnknown(t *testing.T) {
	now := time.Now()
	// rowan's reset has passed (refreshable). ID starts with 'r'
	rowan := quotaResetTestAuth("rowan", nil, now)
	rowan.Provider = "antigravity"
	rowan.Metadata = map[string]any{
		"weekly_quota_reset_at": now.Add(-5 * time.Minute).UTC().Format(time.RFC3339),
	}
	// zach has no reset signals (active unknown). ID starts with 'z', sorts after 'rowan'
	zach := quotaResetTestAuth("zach", nil, now)
	zach.Provider = "antigravity"
	zach.ModelStates = nil

	// rowan sorts before zach by ID, but because rowan is refreshable (new cycle started, reset furthest away),
	// rowan must be placed last (after active unknown zach).
	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{rowan, zach})
	if err != nil || got == nil || got.ID != "zach" {
		t.Fatalf("Pick = %v, %v; want zach (active unknown beats refreshable rowan)", got, err)
	}
}

func TestQuotaResetFirstSelector_RefreshableModelBucketPlacedLast(t *testing.T) {
	now := time.Now()
	rowan := quotaResetTestAuth("rowan", nil, now)
	kevin := quotaResetTestAuth("kevin", nil, now)
	rowan.Provider = "antigravity"
	kevin.Provider = "antigravity"
	// rowan's model bucket reset passed 5m ago
	rowan.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now.Add(-10 * time.Minute),
				Signals: map[string]string{
					"reset_time":         now.Add(-5 * time.Minute).Format(time.RFC3339),
					"remaining_fraction": "0.8",
				},
			},
		},
	}
	// kevin's model bucket resets in 1h
	kevin.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":         now.Add(1 * time.Hour).Format(time.RFC3339),
					"remaining_fraction": "0.8",
				},
			},
		},
	}

	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{rowan, kevin})
	if err != nil || got == nil || got.ID != "kevin" {
		t.Fatalf("Pick = %v, %v; want kevin (rowan bucket is refreshable)", got, err)
	}
}

func TestQuotaResetFirstSelector_AllRefreshableStillSelects(t *testing.T) {
	now := time.Now()
	rowan := quotaResetTestAuth("rowan", nil, now)
	kevin := quotaResetTestAuth("kevin", nil, now)
	rowan.Provider = "antigravity"
	kevin.Provider = "antigravity"
	rowan.Metadata = map[string]any{"weekly_quota_reset_at": now.Add(-10 * time.Minute).UTC().Format(time.RFC3339)}
	kevin.Metadata = map[string]any{"weekly_quota_reset_at": now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)}

	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{rowan, kevin})
	if err != nil || got == nil {
		t.Fatalf("Pick(all refreshable) returned nil/error: %v, %v", got, err)
	}
}

func TestQuotaResetFirstSelector_NotifiesRefreshableAuthOnRequest(t *testing.T) {
	now := time.Now()
	rowan := quotaResetTestAuth("rowan", nil, now)
	kevin := quotaResetTestAuth("kevin", nil, now)
	rowan.Provider = "antigravity"
	kevin.Provider = "antigravity"
	// rowan's weekly reset passed 5m ago (refreshable)
	rowan.Metadata = map[string]any{"weekly_quota_reset_at": now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)}
	// kevin resets in 2h (unexpired future reset)
	kevin.Metadata = map[string]any{"weekly_quota_reset_at": now.Add(2 * time.Hour).UTC().Format(time.RFC3339)}

	var notifiedID string
	selector := &QuotaResetFirstSelector{
		OnRefreshableAuth: func(auth *Auth, observedNow time.Time) {
			notifiedID = auth.ID
		},
	}

	got, err := selector.Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{rowan, kevin})
	if err != nil || got == nil || got.ID != "kevin" {
		t.Fatalf("Pick = %v, %v; want kevin", got, err)
	}
	if notifiedID != "rowan" {
		t.Fatalf("expected OnRefreshableAuth notified for rowan, got %q", notifiedID)
	}

	// Negative case: when both have future resets, OnRefreshableAuth must not be called
	notifiedID = ""
	rowan.Metadata = map[string]any{"weekly_quota_reset_at": now.Add(5 * time.Hour).UTC().Format(time.RFC3339)}
	_, err = selector.Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{rowan, kevin})
	if err != nil {
		t.Fatal(err)
	}
	if notifiedID != "" {
		t.Fatalf("expected no notification for unexpired auths, got %q", notifiedID)
	}
}

func TestQuotaResetFirstSelector_WeeklyResetPrecedesFiveHourRollingBucket(t *testing.T) {
	now := time.Now()
	// tong has a weekly reset in 20 hours, and a 5-hour rolling bucket reset in 4 hours
	tong := quotaResetTestAuth("tong", nil, now)
	tong.Provider = "antigravity"
	tong.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":            now.Add(4 * time.Hour).Format(time.RFC3339),
					"weekly_quota_reset_at": now.Add(20 * time.Hour).Format(time.RFC3339),
					"remaining_fraction":    "0.8",
				},
			},
		},
	}

	// rowan has a weekly reset in 7 days, but made a request recently so 5-hour rolling bucket resets in 1 hour
	rowan := quotaResetTestAuth("rowan", nil, now)
	rowan.Provider = "antigravity"
	rowan.ModelStates = map[string]*ModelState{
		"gemini-3.8-flash-tiered": {
			Status: StatusActive,
			Quota: QuotaState{
				ObservedAt: now,
				Signals: map[string]string{
					"reset_time":            now.Add(1 * time.Hour).Format(time.RFC3339),
					"weekly_quota_reset_at": now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
					"remaining_fraction":    "0.6",
				},
			},
		},
	}

	// The selector must prefer tong (weekly reset in 20h) over rowan (weekly reset in 7d).
	// Rowan's shorter rolling 5h bucket (1h) must NOT cause rowan to be preferred over tong.
	got, err := (&QuotaResetFirstSelector{}).Pick(context.Background(), "antigravity", "gemini-3.8-flash-high", cliproxyexecutor.Options{}, []*Auth{rowan, tong})
	if err != nil || got == nil || got.ID != "tong" {
		t.Fatalf("Pick = %v, %v; want tong (weekly reset 20h outranks 7d despite 5h bucket)", got, err)
	}
}
