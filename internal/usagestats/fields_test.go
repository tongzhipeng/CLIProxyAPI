package usagestats

import (
	"context"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestHandleUsageCapturesEndpointAndReasoningEffort(t *testing.T) {
	logsDir := t.TempDir()
	Configure(true, logsDir)
	defer Configure(false, "")

	ctx := context.Background()
	ctx = internallogging.WithEndpoint(ctx, "POST /v1/chat/completions")
	ctx = coreusage.WithReasoningEffort(ctx, "medium")

	// Case 1: effort from record takes priority over context
	record1 := coreusage.Record{
		RequestedAt:     time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Provider:        "anthropic",
		Model:           "claude-sonnet-5",
		ReasoningEffort: "high",
		Latency:         150 * time.Millisecond,
		TTFT:            30 * time.Millisecond,
	}
	(&recordPlugin{}).HandleUsage(ctx, record1)

	// Case 2: effort falls back to context when record's is empty
	record2 := coreusage.Record{
		RequestedAt: time.Date(2026, 9, 18, 12, 5, 0, 0, time.UTC),
		Provider:    "anthropic",
		Model:       "claude-sonnet-5",
		Latency:     200 * time.Millisecond,
	}
	(&recordPlugin{}).HandleUsage(ctx, record2)

	statsDir := Dir(logsDir)
	page, err := QueryRecords(statsDir, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 18, 23, 59, 59, 0, time.UTC), RecordFilter{}, 0, 10)
	if err != nil {
		t.Fatalf("QueryRecords: %v", err)
	}
	if len(page.Records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(page.Records))
	}

	// newest first: record2 then record1
	r2 := page.Records[0]
	r1 := page.Records[1]

	if r1.Endpoint != "POST /v1/chat/completions" {
		t.Errorf("r1.Endpoint = %q, want 'POST /v1/chat/completions'", r1.Endpoint)
	}
	if r1.ReasoningEffort != "high" {
		t.Errorf("r1.ReasoningEffort = %q, want 'high'", r1.ReasoningEffort)
	}

	if r2.Endpoint != "POST /v1/chat/completions" {
		t.Errorf("r2.Endpoint = %q, want 'POST /v1/chat/completions'", r2.Endpoint)
	}
	if r2.ReasoningEffort != "medium" {
		t.Errorf("r2.ReasoningEffort = %q, want 'medium' (from ctx fallback)", r2.ReasoningEffort)
	}
}

func TestQueryRecordsHandlesLegacyLinesWithoutNewFields(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "2026-09-18", []string{
		`{"timestamp":"2026-09-18T10:00:00Z","provider":"antigravity","model":"gemini","account":"a","failed":false,"status_code":200,"token_breakdown":{"total_tokens":10}}`,
	})
	page, err := QueryRecords(dir, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 18, 23, 59, 59, 0, time.UTC), RecordFilter{}, 0, 10)
	if err != nil {
		t.Fatalf("QueryRecords: %v", err)
	}
	if len(page.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(page.Records))
	}
	if page.Records[0].Endpoint != "" {
		t.Errorf("expected empty Endpoint on legacy record, got %q", page.Records[0].Endpoint)
	}
	if page.Records[0].ReasoningEffort != "" {
		t.Errorf("expected empty ReasoningEffort on legacy record, got %q", page.Records[0].ReasoningEffort)
	}
}
