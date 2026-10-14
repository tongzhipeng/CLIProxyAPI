package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestAntigravityQuotaRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var calls atomic.Int32
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1internal:retrieveUserQuota" || string(body) != "{}" {
			t.Errorf("unexpected quota request: %s %s %s", r.Method, r.URL.Path, body)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing quota request headers")
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"buckets":[{"modelId":"gemini-test","remainingFraction":0.6,"resetTime":%q,"tokenType":"WTUS"}]}`, now.Add(time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()
	manager := coreauth.NewManager(nil, nil, nil)
	a := &coreauth.Auth{ID: "quota-refresh", Provider: "antigravity", Status: coreauth.StatusActive,
		Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "test-token"}}
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: &config.Config{Routing: internalconfig.RoutingConfig{Strategy: "quota-reset-first"}}, coreManager: manager}
	next := make(map[string]antigravityQuotaRefreshEntry)
	s.refreshAntigravityQuotas(context.Background(), next, now)
	got, _ := manager.GetByID(a.ID)
	if calls.Load() != 1 || got.ModelStates["gemini-test"] == nil {
		t.Fatal("quota was not fetched and stored")
	}
	q := got.ModelStates["gemini-test"].Quota
	if q.Signals["remaining_fraction"] != "0.6" || !q.ObservedAt.Equal(now) {
		t.Fatalf("quota = %+v", q)
	}
	s.refreshAntigravityQuotas(context.Background(), next, now.Add(time.Minute))
	if calls.Load() != 1 {
		t.Fatal("quota refreshed before TTL")
	}
	fail.Store(true)
	s.refreshAntigravityQuotas(context.Background(), next, now.Add(5*time.Minute))
	got, _ = manager.GetByID(a.ID)
	if calls.Load() != 2 || !got.ModelStates["gemini-test"].Quota.ObservedAt.Equal(now) {
		t.Fatal("failed probe replaced valid observation")
	}
	s.refreshAntigravityQuotas(context.Background(), next, now.Add(5*time.Minute+30*time.Second))
	if calls.Load() != 2 {
		t.Fatal("failed probe ignored retry backoff")
	}
	fail.Store(false)
	s.refreshAntigravityQuotas(context.Background(), next, now.Add(6*time.Minute))
	if calls.Load() != 3 {
		t.Fatal("failed probe did not retry")
	}
	// Hot reload out of this strategy suppresses all new quota probes.
	s.cfg = &config.Config{Routing: internalconfig.RoutingConfig{Strategy: "fill-first"}}
	s.refreshAntigravityQuotas(context.Background(), next, now.Add(time.Hour))
	if calls.Load() != 3 {
		t.Fatal("non-quota strategy fetched quota")
	}
	s.cfg = &config.Config{Routing: internalconfig.RoutingConfig{Strategy: "qrf"}}
	current, _ := manager.GetByID(a.ID)
	current.Disabled = true
	if _, err := manager.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	s.refreshAntigravityQuotas(context.Background(), next, now.Add(time.Hour))
	if calls.Load() != 3 || len(next) != 0 {
		t.Fatal("disabled auth was probed or retained in refresh cache")
	}
}

func TestParseAntigravityQuotas(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, body string
		count      int
		valid      bool
	}{
		{"valid", `{"buckets":[{"modelId":"gemini-test","remainingFraction":0.5,"resetTime":"2026-10-09T02:00:00Z"}]}`, 1, true},
		{"empty", `{"buckets":[]}`, 0, true},
		{"missing-reset", `{"buckets":[{"modelId":"unused","remainingFraction":1},{"modelId":"gemini-test","remainingFraction":0.5,"resetTime":"2026-10-09T02:00:00Z"}]}`, 1, true},
		{"missing", `{}`, 0, false},
		{"malformed", `{"buckets":`, 0, false},
		{"invalid-time", `{"buckets":[{"modelId":"gemini-test","resetTime":"bad"}]}`, 0, false},
		{"invalid-fraction", `{"buckets":[{"modelId":"gemini-test","remainingFraction":2,"resetTime":"2026-10-09T02:00:00Z"}]}`, 0, false},
		{"expired", `{"buckets":[{"modelId":"gemini-test","remainingFraction":0,"resetTime":"2026-10-08T02:00:00Z"}]}`, 0, true},
		{"tiered-expansion", `{"buckets":[{"modelId":"gemini-3.8-flash-tiered","remainingFraction":0.8,"resetTime":"2026-10-09T02:00:00Z"}]}`, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quotas, err := parseAntigravityQuotas([]byte(tc.body), now)
			if (err == nil) != tc.valid || len(quotas) != tc.count {
				t.Fatalf("count=%d err=%v", len(quotas), err)
			}
		})
	}
}

func TestAntigravityQuotaWorkerCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	manager := coreauth.NewManager(nil, nil, nil)
	a := &coreauth.Auth{ID: "cancel-quota", Provider: "antigravity", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": "token"}}
	if _, err := manager.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	s := &Service{coreManager: manager, cfg: &config.Config{Routing: internalconfig.RoutingConfig{Strategy: "qrf"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.runAntigravityQuotaRefresh(ctx, make(chan time.Time)) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not probe at startup")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

func TestAntigravityQuotaRefreshAccountIsolation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hours := 4
		if r.Header.Get("Authorization") == "Bearer second" {
			hours = 2
		}
		fmt.Fprintf(w, `{"buckets":[{"modelId":"gemini-test","remainingFraction":0.7,"resetTime":%q}]}`, now.Add(time.Duration(hours)*time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()
	m := coreauth.NewManager(nil, nil, nil)
	for _, id := range []string{"first", "second"} {
		if _, err := m.Register(context.Background(), &coreauth.Auth{ID: id, Provider: "antigravity", Attributes: map[string]string{"base_url": server.URL}, Metadata: map[string]any{"access_token": id}}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{coreManager: m, cfg: &config.Config{Routing: internalconfig.RoutingConfig{Strategy: "qrf"}}}
	s.refreshAntigravityQuotas(context.Background(), make(map[string]antigravityQuotaRefreshEntry), now)
	first, _ := m.GetByID("first")
	second, _ := m.GetByID("second")
	if first.ModelStates["gemini-test"].Quota.Signals["reset_time"] == second.ModelStates["gemini-test"].Quota.Signals["reset_time"] {
		t.Fatal("account quotas were shared")
	}
}

func TestAntigravityQuotaRefresh_PeriodicTimerSkipsRefreshableAccount(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var probeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalls.Add(1)
		fmt.Fprintf(w, `{"buckets":[{"modelId":"gemini-test","remainingFraction":1.0,"resetTime":%q}]}`, now.Add(time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()

	m := coreauth.NewManager(nil, nil, nil)
	rowan := &coreauth.Auth{
		ID:         "rowan",
		Provider:   "antigravity",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token":          "rowan-token",
			"weekly_quota_reset_at": now.Add(-5 * time.Minute).Format(time.RFC3339),
		},
	}
	if _, err := m.Register(context.Background(), rowan); err != nil {
		t.Fatal(err)
	}

	s := &Service{coreManager: m, cfg: &config.Config{Routing: internalconfig.RoutingConfig{Strategy: "quota-reset-first"}}}
	// Entry exists for rowan with matching RegistrationEpoch, but rowan's reset time has passed (refreshable).
	tokenHash := [32]byte{}
	next := map[string]antigravityQuotaRefreshEntry{
		"rowan": {
			next:      now.Add(-time.Second),
			epoch:     rowan.RegistrationEpoch,
			tokenHash: tokenHash,
		},
	}

	// Periodic timer tick runs: it MUST NOT probe rowan on a timer because rowan is refreshable
	s.refreshAntigravityQuotas(context.Background(), next, now)

	if probeCalls.Load() != 0 {
		t.Fatalf("expected 0 probe calls from periodic timer for refreshable account, got %d", probeCalls.Load())
	}
}

func TestAntigravityQuotaRefresh_RequestTriggeredProbeWithJitter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	newReset := now.Add(7 * 24 * time.Hour) // New cycle reset in 7 days
	var probeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCalls.Add(1)
		fmt.Fprintf(w, `{"buckets":[{"modelId":"gemini-test","remainingFraction":1.0,"resetTime":%q}]}`, newReset.Format(time.RFC3339))
	}))
	defer server.Close()

	// Injected short deterministic 30ms jitter for testing
	origJitter := antigravityQuotaJitterFunc
	antigravityQuotaJitterFunc = func(id string, base time.Time) time.Duration {
		return 30 * time.Millisecond
	}
	defer func() { antigravityQuotaJitterFunc = origJitter }()

	m := coreauth.NewManager(nil, nil, nil)
	rowan := &coreauth.Auth{
		ID:         "rowan",
		Provider:   "antigravity",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"base_url": server.URL},
		Metadata: map[string]any{
			"access_token":          "rowan-token",
			"weekly_quota_reset_at": now.Add(-5 * time.Minute).Format(time.RFC3339),
		},
	}
	if _, err := m.Register(context.Background(), rowan); err != nil {
		t.Fatal(err)
	}

	s := &Service{coreManager: m, cfg: &config.Config{Routing: internalconfig.RoutingConfig{Strategy: "quota-reset-first"}}}

	// Request arrives and discovers rowan is in refreshable time -> triggers delayed refresh
	s.handleRefreshableAuth(rowan, now)

	// Immediately, probe must NOT have fired yet (must wait for jitter delay)
	if probeCalls.Load() != 0 {
		t.Fatalf("expected 0 probe calls immediately, got %d", probeCalls.Load())
	}

	// Multiple requests during jitter delay do not trigger duplicate probes
	s.handleRefreshableAuth(rowan, now.Add(10*time.Millisecond))
	s.handleRefreshableAuth(rowan, now.Add(20*time.Millisecond))

	// Wait for the scheduled probe to execute
	s.antigravityProbeWg.Wait()

	if probeCalls.Load() != 1 {
		t.Fatalf("expected exactly 1 probe call, got %d", probeCalls.Load())
	}

	got, _ := m.GetByID("rowan")
	state := got.ModelStates["gemini-test"]
	if state == nil {
		t.Fatal("gemini-test state not observed")
	}
	// Verify that real quota fraction and real reset time were restored
	if state.Quota.Signals["remaining_fraction"] != "1" {
		t.Fatalf("expected remaining_fraction 1, got %s", state.Quota.Signals["remaining_fraction"])
	}
	if state.Quota.Signals["reset_time"] != newReset.Format(time.RFC3339Nano) {
		t.Fatalf("expected reset_time %s, got %s", newReset.Format(time.RFC3339Nano), state.Quota.Signals["reset_time"])
	}
}
