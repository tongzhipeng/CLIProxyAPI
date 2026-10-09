package auth

import (
	"context"
	"testing"
	"time"
)

func TestObserveAntigravityQuotasPreservesRuntimeState(t *testing.T) {
	m := NewManager(nil, nil, nil)
	now := time.Now()
	a := &Auth{ID: "quota-observation", Provider: "antigravity", Metadata: map[string]any{"access_token": "test"}, ModelStates: map[string]*ModelState{
		"gemini-test": {Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(time.Hour)}},
	}}
	base, err := m.Register(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	observations := map[string]QuotaState{"gemini-test": {ObservedAt: now, Signals: map[string]string{"reset_time": now.Add(2 * time.Hour).Format(time.RFC3339)}}}
	if !m.ObserveAntigravityQuotas(base, observations, now) {
		t.Fatal("observation rejected")
	}
	if m.ObserveAntigravityQuotas(base, nil, now.Add(-time.Minute)) {
		t.Fatal("late snapshot replaced a newer observation")
	}
	observations["gemini-test"].Signals["reset_time"] = "mutated"
	got, _ := m.GetByID(a.ID)
	state := got.ModelStates["gemini-test"]
	if !state.Unavailable || !state.Quota.Exceeded || !state.NextRetryAfter.Equal(now.Add(time.Hour)) || state.Quota.Signals["reset_time"] == "mutated" {
		t.Fatalf("runtime state changed: %+v", state)
	}
	if !m.ObserveAntigravityQuotas(base, nil, now) {
		t.Fatal("empty snapshot rejected")
	}
	got, _ = m.GetByID(a.ID)
	if len(got.ModelStates["gemini-test"].Quota.Signals) != 0 || !got.ModelStates["gemini-test"].Quota.Exceeded {
		t.Fatal("missing bucket retained old observation or erased cooldown")
	}
	if _, err = m.Register(context.Background(), a.Clone()); err != nil {
		t.Fatal(err)
	}
	if m.ObserveAntigravityQuotas(base, observations, now) {
		t.Fatal("stale registration accepted")
	}
}
