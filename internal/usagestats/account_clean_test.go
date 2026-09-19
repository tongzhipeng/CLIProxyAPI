package usagestats

import (
	"testing"
	"time"
)

func TestCleanAccount(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"   ", ""},
		{"antigravity-kevin.lee.rowan@gmail.com.json", "antigravity-kevin.lee.rowan@gmail.com"},
		{"codex-023b6056-tongzhipeng5688@gmail.com-plus.json", "codex-023b6056-tongzhipeng5688@gmail.com-plus"},
		{"antigravity-kevin.lee.rowan@gmail.com", "antigravity-kevin.lee.rowan@gmail.com"},
		{"  my-account.json  ", "my-account"},
		{"my-account.JSON", "my-account.JSON"},
	}

	for _, tc := range tests {
		got := CleanAccount(tc.input)
		if got != tc.want {
			t.Errorf("CleanAccount(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCleanAccountInQueries(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	// Write records: one with .json and one without
	writeFixtureFile(t, dir, "2026-09-18", []string{
		`{"timestamp":"2026-09-18T10:00:00Z","provider":"openai","model":"gpt-4o","account":"user-alpha.json","failed":false,"status_code":200,"token_breakdown":{"total_tokens":100}}`,
		`{"timestamp":"2026-09-18T11:00:00Z","provider":"openai","model":"gpt-4o","account":"user-beta","failed":false,"status_code":200,"token_breakdown":{"total_tokens":200}}`,
	})

	from := now.Add(-24 * time.Hour)
	to := now

	// Query breakdown by account with no filter - should strip .json from keys
	rows, err := QueryBreakdown(dir, from, to, "account", Filter{})
	if err != nil {
		t.Fatalf("QueryBreakdown error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	for _, r := range rows {
		if r.Key == "user-alpha.json" {
			t.Errorf("expected user-alpha.json to be cleaned to user-alpha, got %q", r.Key)
		}
	}

	// Query timeseries filtering by account without .json
	tsClean, err := QueryTimeseries(dir, from, to, "hour", Filter{Account: "user-alpha"})
	if err != nil {
		t.Fatalf("QueryTimeseries clean error: %v", err)
	}
	totalClean := 0
	for _, b := range tsClean {
		totalClean += int(b.TotalTokens)
	}
	if totalClean != 100 {
		t.Errorf("expected 100 tokens for user-alpha query, got %d", totalClean)
	}

	// Query timeseries filtering by account with .json
	tsWithJson, err := QueryTimeseries(dir, from, to, "hour", Filter{Account: "user-alpha.json"})
	if err != nil {
		t.Fatalf("QueryTimeseries json error: %v", err)
	}
	totalWithJson := 0
	for _, b := range tsWithJson {
		totalWithJson += int(b.TotalTokens)
	}
	if totalWithJson != 100 {
		t.Errorf("expected 100 tokens for user-alpha.json query, got %d", totalWithJson)
	}

	// Query records filtering by account with .json
	recPage, err := QueryRecords(dir, from, to, RecordFilter{Account: "user-alpha.json"}, 0, 10)
	if err != nil {
		t.Fatalf("QueryRecords error: %v", err)
	}
	if len(recPage.Records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recPage.Records))
	}
	if recPage.Records[0].Account != "user-alpha" {
		t.Errorf("expected record account to be user-alpha, got %q", recPage.Records[0].Account)
	}
}
