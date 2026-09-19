package usagestats

import (
	"testing"
	"time"
)

func TestQueryBreakdownDimensionsAndSorting(t *testing.T) {
	dir := t.TempDir()

	writeFixtureFile(t, dir, "2026-09-18", []string{
		`{"timestamp":"2026-09-18T10:00:00Z","provider":"p1","model":"m1","endpoint":"POST /v1/chat","account":"acc1","failed":false,"latency_ms":100,"ttft_ms":20,"token_breakdown":{"total_tokens":50,"input":{"uncached_tokens":30},"output":{"non_reasoning_tokens":20}}}`,
		`{"timestamp":"2026-09-18T10:30:00Z","provider":"p2","model":"m2","endpoint":"POST /v1/chat","account":"acc2","failed":true,"latency_ms":50,"token_breakdown":{"total_tokens":100,"input":{"uncached_tokens":80},"output":{"non_reasoning_tokens":20}}}`,
		`{"timestamp":"2026-09-18T11:00:00Z","provider":"p1","model":"m1","endpoint":"POST /v1/responses","account":"acc1","failed":false,"latency_ms":150,"token_breakdown":{"total_tokens":50,"input":{"uncached_tokens":30},"output":{"non_reasoning_tokens":20}}}`,
		`{"timestamp":"2026-09-18T12:00:00Z","provider":"p3","model":"","endpoint":"","account":"acc3","failed":false,"token_breakdown":{"total_tokens":0}}`,
	})

	from := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

	// Dimension: model
	// m1 has total 100 tokens, 2 records
	// m2 has total 100 tokens, 1 record
	// "" has total 0 tokens, 1 record
	// Sorting: TotalTokens DESC -> Records DESC -> Key ASC
	// m1 (100, 2) should precede m2 (100, 1)
	rows, err := QueryBreakdown(dir, from, to, "model", Filter{})
	if err != nil {
		t.Fatalf("QueryBreakdown model: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 model rows, got %d", len(rows))
	}
	if rows[0].Key != "m1" || rows[0].TotalTokens != 100 || rows[0].Records != 2 {
		t.Errorf("rows[0] = %+v, want m1 (100 tokens, 2 records)", rows[0])
	}
	if rows[1].Key != "m2" || rows[1].TotalTokens != 100 || rows[1].Records != 1 {
		t.Errorf("rows[1] = %+v, want m2 (100 tokens, 1 record)", rows[1])
	}
	if rows[2].Key != "" || rows[2].TotalTokens != 0 || rows[2].Records != 1 {
		t.Errorf("rows[2] = %+v, want empty key (0 tokens, 1 record)", rows[2])
	}

	// Dimension: endpoint
	// "POST /v1/chat" -> 150 tokens (m1+m2)
	// "POST /v1/responses" -> 50 tokens
	// "" -> 0 tokens
	epRows, err := QueryBreakdown(dir, from, to, "endpoint", Filter{})
	if err != nil {
		t.Fatalf("QueryBreakdown endpoint: %v", err)
	}
	if len(epRows) != 3 {
		t.Fatalf("expected 3 endpoint rows, got %d", len(epRows))
	}
	if epRows[0].Key != "POST /v1/chat" || epRows[0].TotalTokens != 150 {
		t.Errorf("epRows[0] = %+v, want POST /v1/chat with 150 tokens", epRows[0])
	}

	// Test cross-interface reconciliation:
	// sum of breakdown rows TotalTokens == exact timeseries TotalTokens == records TotalTokens
	tbBuckets, err := QueryTimeseriesWithOptions(dir, from, to, "hour", Filter{}, RangeModeExact)
	if err != nil {
		t.Fatalf("QueryTimeseriesWithOptions: %v", err)
	}
	var tsTokens int64
	for _, b := range tbBuckets {
		tsTokens += b.TotalTokens
	}
	var bdTokens int64
	for _, r := range rows {
		bdTokens += r.TotalTokens
	}
	if tsTokens != bdTokens || tsTokens != 200 {
		t.Errorf("token mismatch: tsTokens=%d bdTokens=%d want 200", tsTokens, bdTokens)
	}

	// Invalid dimension
	_, errInvalid := QueryBreakdown(dir, from, to, "invalid_dim", Filter{})
	if errInvalid == nil {
		t.Errorf("expected error for invalid dimension")
	}

	// from > to error
	_, errFromAfterTo := QueryBreakdown(dir, to, from, "model", Filter{})
	if errFromAfterTo == nil {
		t.Errorf("expected error for from > to")
	}
}
