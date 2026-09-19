package usagestats

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	ErrSnapshotExpired   = errors.New("usagestats: snapshot expired or not found")
	ErrSnapshotMismatch  = errors.New("usagestats: snapshot parameters mismatch")
	ErrSnapshotCorrupted = errors.New("usagestats: snapshot file corrupted or truncated")
)

const (
	defaultSnapshotMaxItems = 64
	defaultSnapshotTTL      = 10 * time.Minute
	maxSnapshotRangeDays    = 90
)

// Snapshot preserves the file reading boundaries across daily JSONL files.
type Snapshot struct {
	ID         string
	CreatedAt  time.Time
	Dir        string
	From       time.Time
	To         time.Time
	Filter     RecordFilter
	Boundaries map[string]int64 // dateStr -> endOffset (valid newline-terminated byte position)
}

// SnapshotStore manages in-memory pagination snapshots.
type SnapshotStore struct {
	mu        sync.Mutex
	snapshots map[string]*Snapshot
	order     []string // LRU ordering, oldest first
	maxItems  int
	ttl       time.Duration
	nowFunc   func() time.Time
}

// NewSnapshotStore creates a new concurrency-safe snapshot store.
func NewSnapshotStore(maxItems int, ttl time.Duration, nowFunc func() time.Time) *SnapshotStore {
	if maxItems <= 0 {
		maxItems = defaultSnapshotMaxItems
	}
	if ttl <= 0 {
		ttl = defaultSnapshotTTL
	}
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &SnapshotStore{
		snapshots: make(map[string]*Snapshot),
		order:     make([]string, 0, maxItems),
		maxItems:  maxItems,
		ttl:       ttl,
		nowFunc:   nowFunc,
	}
}

// DefaultSnapshotStore is the global snapshot store used by the HTTP handler.
var DefaultSnapshotStore = NewSnapshotStore(defaultSnapshotMaxItems, defaultSnapshotTTL, nil)

// CreateSnapshot captures current file boundaries for the given window and filter.
func (s *SnapshotStore) CreateSnapshot(dir string, from, to time.Time, filter RecordFilter) (*Snapshot, error) {
	if from.After(to) {
		return nil, fmt.Errorf("usagestats: from must not be after to")
	}
	if to.Sub(from) > maxSnapshotRangeDays*24*time.Hour {
		return nil, fmt.Errorf("usagestats: snapshot range exceeds %d days", maxSnapshotRangeDays)
	}

	from = from.UTC()
	to = to.UTC()
	startDay := from.Truncate(24 * time.Hour)
	endDay := to.Truncate(24 * time.Hour)

	boundaries := make(map[string]int64)
	for day := startDay; !day.After(endDay); day = day.AddDate(0, 0, 1) {
		dateStr := day.Format(dateLayout)
		path := filepath.Join(dir, "usage-"+dateStr+".jsonl")
		offset, err := findFileEndOffset(path)
		if err != nil {
			return nil, err
		}
		boundaries[dateStr] = offset
	}

	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, fmt.Errorf("usagestats: generate snapshot ID failed: %w", err)
	}
	id := "snap_" + hex.EncodeToString(idBytes)

	now := s.nowFunc().UTC()
	snap := &Snapshot{
		ID:         id,
		CreatedAt:  now,
		Dir:        filepath.Clean(dir),
		From:       from,
		To:         to,
		Filter:     filter,
		Boundaries: boundaries,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(now)

	// Enforce capacity
	if len(s.snapshots) >= s.maxItems && len(s.order) > 0 {
		evictedID := s.order[0]
		s.order = s.order[1:]
		delete(s.snapshots, evictedID)
	}

	s.snapshots[id] = snap
	s.order = append(s.order, id)

	return snap, nil
}

// GetSnapshot retrieves an existing snapshot, verifying TTL and matching parameters.
func (s *SnapshotStore) GetSnapshot(id, dir string, from, to time.Time, filter RecordFilter) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFunc().UTC()
	s.cleanupExpiredLocked(now)

	snap, exists := s.snapshots[id]
	if !exists {
		return nil, ErrSnapshotExpired
	}

	// Validate dir
	if snap.Dir != filepath.Clean(dir) {
		return nil, ErrSnapshotMismatch
	}

	// Validate from and to
	if !snap.From.Equal(from.UTC()) || !snap.To.Equal(to.UTC()) {
		return nil, ErrSnapshotMismatch
	}

	// Validate filter
	if snap.Filter.Provider != filter.Provider ||
		snap.Filter.Model != filter.Model ||
		snap.Filter.Account != filter.Account {
		return nil, ErrSnapshotMismatch
	}
	if (snap.Filter.Failed == nil && filter.Failed != nil) ||
		(snap.Filter.Failed != nil && filter.Failed == nil) ||
		(snap.Filter.Failed != nil && filter.Failed != nil && *snap.Filter.Failed != *filter.Failed) {
		return nil, ErrSnapshotMismatch
	}

	// Verify on-disk files haven't been truncated below snapshot boundaries
	for dateStr, boundary := range snap.Boundaries {
		if boundary <= 0 {
			continue
		}
		path := filepath.Join(dir, "usage-"+dateStr+".jsonl")
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, ErrSnapshotCorrupted
			}
			return nil, fmt.Errorf("usagestats: check %s: %w", path, err)
		}
		if info.Size() < boundary {
			return nil, ErrSnapshotCorrupted
		}
	}

	return snap, nil
}

func (s *SnapshotStore) cleanupExpiredLocked(now time.Time) {
	cutoff := now.Add(-s.ttl)
	newOrder := make([]string, 0, len(s.order))
	for _, id := range s.order {
		snap, ok := s.snapshots[id]
		if !ok {
			continue
		}
		if snap.CreatedAt.Before(cutoff) {
			delete(s.snapshots, id)
		} else {
			newOrder = append(newOrder, id)
		}
	}
	s.order = newOrder
}

// findFileEndOffset returns the byte offset of the last complete line in the file.
func findFileEndOffset(path string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("usagestats: open %s: %w", path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}

	// Check the last byte: if it's '\n', the whole file is complete.
	var lastByte [1]byte
	if _, errRead := file.ReadAt(lastByte[:], size-1); errRead != nil {
		return 0, errRead
	}
	if lastByte[0] == '\n' {
		return size, nil
	}

	// Otherwise, find the last '\n' in the file
	bufSize := int64(4096)
	if bufSize > size {
		bufSize = size
	}
	buf := make([]byte, bufSize)
	offset := size - bufSize
	n, errRead := file.ReadAt(buf, offset)
	if errRead != nil && errRead != io.EOF {
		return 0, errRead
	}
	for i := n - 1; i >= 0; i-- {
		if buf[i] == '\n' {
			return offset + int64(i) + 1, nil
		}
	}

	// If no newline in the last chunk and file is larger, search backward
	for offset > 0 {
		readLen := int64(4096)
		if readLen > offset {
			readLen = offset
		}
		offset -= readLen
		n, err := file.ReadAt(buf[:readLen], offset)
		if err != nil && err != io.EOF {
			return 0, err
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				return offset + int64(i) + 1, nil
			}
		}
	}

	return 0, nil
}
