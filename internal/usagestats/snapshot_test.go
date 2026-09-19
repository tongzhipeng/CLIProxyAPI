package usagestats

import (
	"errors"
	"testing"
	"time"
)

func TestQueryRecordsWithSnapshotPreventsLateAppendDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "2026-09-18", []string{
		`{"timestamp":"2026-09-18T10:45:00Z","model":"older"}`,
		`{"timestamp":"2026-09-18T11:00:00Z","model":"newer"}`,
	})

	from := time.Date(2026, 9, 18, 10, 30, 0, 0, time.UTC)
	to := from.Add(time.Hour)

	store := NewSnapshotStore(64, 10*time.Minute, nil)

	// 1. Create snapshot
	snap, err := store.CreateSnapshot(dir, from, to, RecordFilter{})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// 2. Fetch page 1 (limit 1)
	page1, err := QueryRecordsWithOptions(dir, from, to, RecordFilter{}, 0, 1, snap.Boundaries)
	if err != nil {
		t.Fatalf("QueryRecordsWithOptions page1: %v", err)
	}
	if len(page1.Records) != 1 || page1.Records[0].Model != "newer" {
		t.Fatalf("page1 = %+v, want 1 record 'newer'", page1.Records)
	}
	if !page1.HasMore || page1.NextOff != 1 {
		t.Fatalf("page1 metadata = %+v, want has_more=true next_offset=1", page1)
	}

	// 3. Late append record arriving after page 1
	late := time.Date(2026, 9, 18, 10, 50, 0, 0, time.UTC)
	if err := appendLine(dir, late, []byte("{\"timestamp\":\"2026-09-18T10:50:00Z\",\"model\":\"late-completion\"}\n")); err != nil {
		t.Fatal(err)
	}

	// 4. Fetch page 2 using the same snapshot boundaries (offset = page1.NextOff)
	page2, err := QueryRecordsWithOptions(dir, from, to, RecordFilter{}, page1.NextOff, 1, snap.Boundaries)
	if err != nil {
		t.Fatalf("QueryRecordsWithOptions page2: %v", err)
	}
	if len(page2.Records) != 1 || page2.Records[0].Model != "older" {
		t.Fatalf("page2 = %+v, want 1 record 'older' (NOT 'newer' or 'late-completion')", page2.Records)
	}
	if page2.HasMore {
		t.Fatalf("page2 has_more should be false")
	}
}

func TestQueryRecordsHasMoreProbe(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "2026-09-18", []string{
		`{"timestamp":"2026-09-18T10:00:00Z","model":"m1"}`,
		`{"timestamp":"2026-09-18T10:01:00Z","model":"m2"}`,
	})
	from := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 18, 23, 59, 59, 0, time.UTC)

	// Exactly 2 records, limit=2: HasMore must be false, NextOff=2
	page, err := QueryRecords(dir, from, to, RecordFilter{}, 0, 2)
	if err != nil {
		t.Fatalf("QueryRecords: %v", err)
	}
	if len(page.Records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(page.Records))
	}
	if page.HasMore {
		t.Errorf("expected HasMore=false when total matches == limit, got true")
	}
	if page.NextOff != 2 {
		t.Errorf("expected NextOff=2, got %d", page.NextOff)
	}

	// Add 3rd record: now HasMore must be true, NextOff=2
	if err := appendLine(dir, from, []byte("{\"timestamp\":\"2026-09-18T10:02:00Z\",\"model\":\"m3\"}\n")); err != nil {
		t.Fatal(err)
	}
	page2, err := QueryRecords(dir, from, to, RecordFilter{}, 0, 2)
	if err != nil {
		t.Fatalf("QueryRecords: %v", err)
	}
	if len(page2.Records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(page2.Records))
	}
	if !page2.HasMore {
		t.Errorf("expected HasMore=true when more records exist, got false")
	}
	if page2.NextOff != 2 {
		t.Errorf("expected NextOff=2, got %d", page2.NextOff)
	}
}

func TestSnapshotStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	from := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)

	mockTime := time.Date(2026, 9, 18, 11, 0, 0, 0, time.UTC)
	nowFunc := func() time.Time { return mockTime }

	store := NewSnapshotStore(2, 5*time.Minute, nowFunc)

	s1, err := store.CreateSnapshot(dir, from, to, RecordFilter{Model: "m1"})
	if err != nil {
		t.Fatalf("CreateSnapshot s1: %v", err)
	}

	// Retrieval with exact matching params succeeds
	got, err := store.GetSnapshot(s1.ID, dir, from, to, RecordFilter{Model: "m1"})
	if err != nil {
		t.Fatalf("GetSnapshot s1: %v", err)
	}
	if got.ID != s1.ID {
		t.Errorf("got snapshot ID %s, want %s", got.ID, s1.ID)
	}

	// Retrieval with mismatched filter returns ErrSnapshotMismatch
	_, errMismatch := store.GetSnapshot(s1.ID, dir, from, to, RecordFilter{Model: "m2"})
	if !errors.Is(errMismatch, ErrSnapshotMismatch) {
		t.Errorf("expected ErrSnapshotMismatch, got %v", errMismatch)
	}

	// Advance time past TTL -> ErrSnapshotExpired
	mockTime = mockTime.Add(6 * time.Minute)
	_, errExpired := store.GetSnapshot(s1.ID, dir, from, to, RecordFilter{Model: "m1"})
	if !errors.Is(errExpired, ErrSnapshotExpired) {
		t.Errorf("expected ErrSnapshotExpired, got %v", errExpired)
	}
}
