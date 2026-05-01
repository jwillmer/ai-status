package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// SyncSuppressor breaks the sync ↔ fsnotify echo loop. Whenever the sync
// engine writes a file or removes one, it first records the expected
// follow-up event here; the watchLoop calls `consumeWrite` / `consumeRemove`
// before acting and skips the event if it matches a recent suppression.
//
// Entries auto-expire after 5s — long enough to absorb fsnotify's own
// scheduling latency, short enough that a stale entry can't mask a real
// concurrent edit. The hash field on writes ensures we only suppress the
// exact body the sync engine wrote, not any later edit at the same path.
type SyncSuppressor struct {
	mu      sync.Mutex
	writes  map[string]suppressedWrite
	removes map[string]time.Time
}

type suppressedWrite struct {
	hash   string
	expiry time.Time
}

// suppressWindow is how long a recorded suppression remains live. Tuned
// for fsnotify latency on Linux (sub-ms) plus generous slack for laggy
// filesystems on macOS / Windows.
const suppressWindow = 5 * time.Second

func newSyncSuppressor() *SyncSuppressor {
	return &SyncSuppressor{
		writes:  map[string]suppressedWrite{},
		removes: map[string]time.Time{},
	}
}

// suppressWrite records that the sync engine is about to write `path`
// with body hashing to `hash`. The next fsnotify Write event for that
// path with a matching hash is then ignored.
func (s *SyncSuppressor) suppressWrite(path, hash string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes[path] = suppressedWrite{hash: hash, expiry: time.Now().Add(suppressWindow)}
}

// consume is the watchLoop hook for write events. Returns true if the
// event matches a pending suppression and should be skipped.
func (s *SyncSuppressor) consume(path, hash string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sw, ok := s.writes[path]
	if !ok {
		return false
	}
	if time.Now().After(sw.expiry) {
		delete(s.writes, path)
		return false
	}
	if sw.hash != hash {
		return false
	}
	delete(s.writes, path)
	return true
}

// suppressRemove records that the sync engine (or the move handler) is
// about to remove or rename `path`. The next fsnotify Remove/Rename
// event for that path is then ignored.
func (s *SyncSuppressor) suppressRemove(path string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removes[path] = time.Now().Add(suppressWindow)
}

func (s *SyncSuppressor) consumeRemove(path string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.removes[path]
	if !ok {
		return false
	}
	delete(s.removes, path)
	return time.Now().Before(exp)
}

// syncSuppressor is a process-wide singleton wired into watchLoop and
// the sync engine. nil-tolerant — calls on a nil pointer are no-ops, so
// builds with sync disabled don't need to special-case it.
var syncSuppressor = newSyncSuppressor()

// SyncState persists the cursor used for incremental pulls. Stored in
// `data/sync-state.json`. Kept out of `data/settings.json` because it
// changes on every pull and we don't want to thrash the settings file.
type SyncState struct {
	mu         sync.Mutex
	file       string
	LastSyncAt time.Time `json:"lastSyncAt"`
}

func loadSyncState(file string) *SyncState {
	s := &SyncState{file: file}
	data, err := os.ReadFile(file)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, s)
	return s
}

func (s *SyncState) save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}

func (s *SyncState) get() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.LastSyncAt
}

func (s *SyncState) set(t time.Time) {
	s.mu.Lock()
	s.LastSyncAt = t
	s.mu.Unlock()
	_ = s.save()
}
