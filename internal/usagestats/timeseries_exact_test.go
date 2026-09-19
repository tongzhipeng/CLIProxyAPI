package usagestats

import (
	"testing"
	"time"
)

func TestQueryTimeseriesExactModeAndLatencyMetrics(t *testing.T) {
	dir := t.TempDir()

	// Fixture data on 2026-09-18
	// window [10:30, 11:30]
	// 10:00 - outside window
	// 10:45 - inside window (latency 100ms, ttft 20ms, tokens 2)
	// 11:30 - inside window (latency 0ms -> no sample, ttft 50ms, tokens 4)
	// 11:45 - outside window
	writeFixtureFile(t, dir, "2026-09-18", []string{
		`{"timestamp":"2026-09-18T10:00:00Z","model":"before","token_breakdown":{"total_tokens":1}}`,
		`{"timestamp":"2026-09-18T10:45:00Z","model":"inside","latency_ms":100,"ttft_ms":20,"token_breakdown":{"total_tokens":2,"input":{"uncached_tokens":2}}}`,
		`{"timestamp":"2026-09-18T11:30:00Z","model":"at-end","latency_ms":0,"ttft_ms":50,"token_breakdown":{"total_tokens":4,"output":{"non_reasoning_tokens":4}}}`,
		`{"timestamp":"2026-09-18T11:45:00Z","model":"after","token_breakdown":{"total_tokens":8}}`,
	})

	from := time.Date(2026, 9, 18, 10, 30, 0, 0, time.UTC)
	to := from.Add(time.Hour) // 11:30

	// 1. Legacy bucket mode: covers full hours 10:00 and 11:00 -> 4 records, 15 tokens
	bucketBuckets, err := QueryTimeseriesWithOptions(dir, from, to, "hour", Filter{}, RangeModeBucket)
	if err != nil {
		t.Fatalf("QueryTimeseriesWithOptions bucket: %v", err)
	}
	var bCount, bTokens int64
	for _, b := range bucketBuckets {
		bCount += b.Records
		bTokens += b.TotalTokens
	}
	if bCount != 4 || bTokens != 15 {
		t.Errorf("bucket mode: got records=%d tokens=%d, want records=4 tokens=15", bCount, bTokens)
	}

	// 2. Exact mode: strictly within [from, to] -> 2 records, 6 tokens
	exactBuckets, err := QueryTimeseriesWithOptions(dir, from, to, "hour", Filter{}, RangeModeExact)
	if err != nil {
		t.Fatalf("QueryTimeseriesWithOptions exact: %v", err)
	}
	var eCount, eTokens, latencySum, latencySamples, ttftSum, ttftSamples int64
	for _, b := range exactBuckets {
		eCount += b.Records
		eTokens += b.TotalTokens
		latencySum += b.LatencyMsSum
		latencySamples += b.LatencySamples
		ttftSum += b.TtftMsSum
		ttftSamples += b.TtftSamples
	}
	if eCount != 2 || eTokens != 6 {
		t.Errorf("exact mode: got records=%d tokens=%d, want records=2 tokens=6", eCount, eTokens)
	}
	if latencySum != 100 || latencySamples != 1 {
		t.Errorf("latency: got sum=%d samples=%d, want sum=100 samples=1 (latency 0 must be ignored)", latencySum, latencySamples)
	}
	if ttftSum != 70 || ttftSamples != 2 {
		t.Errorf("ttft: got sum=%d samples=%d, want sum=70 samples=2", ttftSum, ttftSamples)
	}

	// 3. from > to must return error in exact mode
	_, errInvalid := QueryTimeseriesWithOptions(dir, to, from, "hour", Filter{}, RangeModeExact)
	if errInvalid == nil {
		t.Errorf("expected error when from > to in exact mode")
	}
}
