package usagestats

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestForEachEntryReadsLargeLinesAndHandlesErrors(t *testing.T) {
	dir := t.TempDir()

	// 200 KiB valid entry
	largePadding := strings.Repeat("a", 200*1024)
	largeLine := fmt.Sprintf(`{"timestamp":"2026-09-18T10:00:00Z","model":"%s","token_breakdown":{"total_tokens":10}}`, largePadding)

	// Normal entry
	normalLine := `{"timestamp":"2026-09-18T10:05:00Z","model":"normal","token_breakdown":{"total_tokens":5}}`

	// Empty line & bad json line
	writeFixtureFile(t, dir, "2026-09-18", []string{
		"",
		largeLine,
		"corrupt-json",
		normalLine,
	})

	day := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	var count int
	var totalTokens int64
	err := forEachEntry(dir, day, day, func(fileDay time.Time, e entry) bool {
		count++
		totalTokens += e.TokenBreakdown.TotalTokens
		return false
	})
	if err != nil {
		t.Fatalf("forEachEntry failed on 200KiB line: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 valid entries (skipping empty and bad JSON), got %d", count)
	}
	if totalTokens != 15 {
		t.Fatalf("expected 15 total tokens, got %d", totalTokens)
	}
}

func TestForEachEntryErrorsOnExceedingMaxLineSize(t *testing.T) {
	dir := t.TempDir()

	// > 1 MiB line (1024*1024 + 10 bytes)
	hugePadding := strings.Repeat("b", 1024*1024+10)
	hugeLine := fmt.Sprintf(`{"timestamp":"2026-09-18T10:00:00Z","model":"%s"}`, hugePadding)

	writeFixtureFile(t, dir, "2026-09-18", []string{hugeLine})

	day := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	err := forEachEntry(dir, day, day, func(fileDay time.Time, e entry) bool {
		return false
	})
	if err == nil {
		t.Fatal("expected error when reading line > 1 MiB, got nil")
	}
}
