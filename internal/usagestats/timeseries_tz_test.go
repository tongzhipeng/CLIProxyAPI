package usagestats

import (
	"testing"
	"time"
)

// TestQueryTimeseriesDayBucket_RespectsCallerTimezone verifies that day-step
// bucketing groups entries by the calendar day in the caller's from.Location(),
// not by UTC calendar day. An entry stored as UTC 2026-09-20T16:30:00Z is
// 2026-09-21T00:30:00+08:00 in Asia/Shanghai, so under a +08:00 from/to it
// must fall into the 2026-09-21 local-day bucket, while an entry at
// 2026-09-20T10:00:00Z (2026-09-20T18:00+08:00) stays in the 2026-09-20 bucket.
func TestQueryTimeseriesDayBucket_RespectsCallerTimezone(t *testing.T) {
	dir := t.TempDir()

	writeFixtureFile(t, dir, "2026-09-20", []string{
		`{"timestamp":"2026-09-20T10:00:00Z","model":"day20-local","token_breakdown":{"total_tokens":10}}`,
		`{"timestamp":"2026-09-20T16:30:00Z","model":"day21-local","token_breakdown":{"total_tokens":20}}`,
	})
	writeFixtureFile(t, dir, "2026-09-21", []string{
		`{"timestamp":"2026-09-21T10:00:00Z","model":"day21-local-2","token_breakdown":{"total_tokens":40}}`,
	})

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}

	// Local window covering the full local days of 2026-09-20 and 2026-09-21.
	from := time.Date(2026, 9, 20, 0, 0, 0, 0, loc)
	to := time.Date(2026, 9, 21, 23, 59, 59, 0, loc)

	buckets, err := QueryTimeseriesWithOptions(dir, from, to, "day", Filter{}, RangeModeExact)
	if err != nil {
		t.Fatalf("QueryTimeseriesWithOptions: %v", err)
	}

	tokensByDay := map[string]int64{}
	recordsByDay := map[string]int64{}
	for _, b := range buckets {
		ts, errParse := time.Parse(time.RFC3339, b.Timestamp)
		if errParse != nil {
			t.Fatalf("parse bucket timestamp %q: %v", b.Timestamp, errParse)
		}
		key := ts.In(loc).Format("2006-01-02")
		tokensByDay[key] += b.TotalTokens
		recordsByDay[key] += b.Records
	}

	if tokensByDay["2026-09-20"] != 10 || recordsByDay["2026-09-20"] != 1 {
		t.Errorf("2026-09-20 local bucket: got tokens=%d records=%d, want tokens=10 records=1",
			tokensByDay["2026-09-20"], recordsByDay["2026-09-20"])
	}
	if tokensByDay["2026-09-21"] != 60 || recordsByDay["2026-09-21"] != 2 {
		t.Errorf("2026-09-21 local bucket: got tokens=%d records=%d, want tokens=60 records=2 (16:30Z entry must roll into local next day)",
			tokensByDay["2026-09-21"], recordsByDay["2026-09-21"])
	}
}

// TestQueryTimeseriesHourBucket_CarriesCallerOffset verifies that hour-step
// bucket timestamps are formatted with the caller's UTC offset rather than
// being silently normalized to Z, and that boundary alignment/aggregation
// still works correctly when from/to carry a +08:00 offset.
func TestQueryTimeseriesHourBucket_CarriesCallerOffset(t *testing.T) {
	dir := t.TempDir()

	writeFixtureFile(t, dir, "2026-09-20", []string{
		`{"timestamp":"2026-09-20T10:15:00Z","model":"m1","token_breakdown":{"total_tokens":5}}`,
		`{"timestamp":"2026-09-20T10:45:00Z","model":"m1","token_breakdown":{"total_tokens":7}}`,
	})

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}

	// 10:15Z-10:45Z window is 18:15+08:00-18:45+08:00 local.
	from := time.Date(2026, 9, 20, 18, 0, 0, 0, loc)
	to := time.Date(2026, 9, 20, 18, 59, 59, 0, loc)

	buckets, err := QueryTimeseriesWithOptions(dir, from, to, "hour", Filter{}, RangeModeExact)
	if err != nil {
		t.Fatalf("QueryTimeseriesWithOptions: %v", err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected 1 hour bucket, got %d: %+v", len(buckets), buckets)
	}
	if buckets[0].TotalTokens != 12 || buckets[0].Records != 2 {
		t.Errorf("bucket totals: got tokens=%d records=%d, want tokens=12 records=2", buckets[0].TotalTokens, buckets[0].Records)
	}
	if got := buckets[0].Timestamp; got[len(got)-6:] != "+08:00" {
		t.Errorf("bucket timestamp = %q, want it to carry +08:00 offset (not Z)", got)
	}
}

// TestQueryBreakdown_TimezoneEquivalentWindowsAgree verifies that an
// absolute time window expressed as +08:00 local times yields the same
// breakdown row counts/totals as the UTC-equivalent instants, since
// breakdown filtering compares absolute instants, not local calendar days.
func TestQueryBreakdown_TimezoneEquivalentWindowsAgree(t *testing.T) {
	dir := t.TempDir()

	writeFixtureFile(t, dir, "2026-09-20", []string{
		`{"timestamp":"2026-09-20T16:00:00Z","model":"gpt-4o","token_breakdown":{"total_tokens":100}}`,
	})
	writeFixtureFile(t, dir, "2026-09-21", []string{
		`{"timestamp":"2026-09-21T01:00:00Z","model":"gpt-4o","token_breakdown":{"total_tokens":200}}`,
	})

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}

	utcFrom := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	utcTo := time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC)
	localFrom := utcFrom.In(loc)
	localTo := utcTo.In(loc)

	rowsUTC, err := QueryBreakdown(dir, utcFrom, utcTo, "model", Filter{})
	if err != nil {
		t.Fatalf("QueryBreakdown utc: %v", err)
	}
	rowsLocal, err := QueryBreakdown(dir, localFrom, localTo, "model", Filter{})
	if err != nil {
		t.Fatalf("QueryBreakdown local: %v", err)
	}

	if len(rowsUTC) != 1 || len(rowsLocal) != 1 {
		t.Fatalf("expected 1 row each, got utc=%d local=%d", len(rowsUTC), len(rowsLocal))
	}
	if rowsUTC[0].TotalTokens != rowsLocal[0].TotalTokens || rowsUTC[0].TotalTokens != 300 {
		t.Errorf("token totals differ or wrong: utc=%d local=%d, want 300 for both", rowsUTC[0].TotalTokens, rowsLocal[0].TotalTokens)
	}
}

// TestQueryRecords_TimezoneEquivalentWindowsAgree verifies that
// QueryRecordsWithOptions returns the same page of records for an absolute
// window regardless of whether from/to are expressed in UTC or a +08:00
// local Location, since record filtering compares absolute instants.
func TestQueryRecords_TimezoneEquivalentWindowsAgree(t *testing.T) {
	dir := t.TempDir()

	writeFixtureFile(t, dir, "2026-09-20", []string{
		`{"timestamp":"2026-09-20T16:00:00Z","model":"m1"}`,
	})
	writeFixtureFile(t, dir, "2026-09-21", []string{
		`{"timestamp":"2026-09-21T01:00:00Z","model":"m2"}`,
	})

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}

	utcFrom := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	utcTo := time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC)
	localFrom := utcFrom.In(loc)
	localTo := utcTo.In(loc)

	pageUTC, err := QueryRecordsWithOptions(dir, utcFrom, utcTo, RecordFilter{}, 0, 50, nil)
	if err != nil {
		t.Fatalf("QueryRecordsWithOptions utc: %v", err)
	}
	pageLocal, err := QueryRecordsWithOptions(dir, localFrom, localTo, RecordFilter{}, 0, 50, nil)
	if err != nil {
		t.Fatalf("QueryRecordsWithOptions local: %v", err)
	}

	if len(pageUTC.Records) != 2 || len(pageLocal.Records) != 2 {
		t.Fatalf("expected 2 records each, got utc=%d local=%d", len(pageUTC.Records), len(pageLocal.Records))
	}
}
