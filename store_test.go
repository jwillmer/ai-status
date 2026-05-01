package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newStore builds a Store rooted at a temp dir and primes it with the
// migration step so individual tests can assert on the post-load state.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	sessRoot := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessRoot, 0755); err != nil {
		t.Fatal(err)
	}
	s := &Store{file: filepath.Join(dir, "sessions.json"), sessRoot: sessRoot}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	return s, sessRoot
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestRelPathFor(t *testing.T) {
	root := "/tmp/sessions"
	cases := map[string]string{
		"/tmp/sessions/plan.md":         "plan.md",
		"/tmp/sessions/sub/plan.md":     "sub/plan.md",
		"/tmp/elsewhere/plan.md":        "",
		"/tmp/sessions/../escape.md":    "",
		"":                              "",
	}
	for in, want := range cases {
		if got := relPathFor(in, root); got != want {
			t.Errorf("relPathFor(%q,%q) = %q, want %q", in, root, got, want)
		}
	}
}

func TestStoreAddFillsSyncFields(t *testing.T) {
	s, root := newTestStore(t)
	path := filepath.Join(root, "plan.md")
	writeFile(t, path, "hello")

	if err := s.add(Session{ID: "abc", Title: "Plan", Path: path, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.byID("abc")
	if !ok {
		t.Fatal("session not found after add")
	}
	if got.RelPath != "plan.md" {
		t.Errorf("RelPath = %q, want plan.md", got.RelPath)
	}
	if got.BodyHash != hashBytes([]byte("hello")) {
		t.Errorf("BodyHash mismatch: %q", got.BodyHash)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set on add")
	}
}

func TestStoreRemoveIsSoftDelete(t *testing.T) {
	s, root := newTestStore(t)
	path := filepath.Join(root, "plan.md")
	writeFile(t, path, "hello")
	_ = s.add(Session{ID: "abc", Title: "Plan", Path: path, Created: time.Now()})

	if _, err := s.remove("abc"); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.byID("abc"); ok {
		t.Error("byID should not return tombstoned row")
	}
	if _, ok := s.byPath(path); ok {
		t.Error("byPath should not return tombstoned row")
	}
	if got := len(s.list()); got != 0 {
		t.Errorf("list() = %d rows, want 0 (tombstone hidden)", got)
	}
	if got := len(s.listAll()); got != 1 {
		t.Errorf("listAll() = %d rows, want 1 (tombstone retained)", got)
	}
	t1, ok := s.tombstoneByRelPath("plan.md")
	if !ok {
		t.Fatal("tombstoneByRelPath should find the tombstone")
	}
	if t1.DeletedAt == nil {
		t.Error("tombstone should have DeletedAt set")
	}
}

func TestStoreResurrect(t *testing.T) {
	s, root := newTestStore(t)
	path := filepath.Join(root, "plan.md")
	writeFile(t, path, "hello")
	_ = s.add(Session{ID: "abc", Title: "Plan", Path: path, Created: time.Now()})
	_, _ = s.remove("abc")

	if err := s.resurrect("abc", path, "world"); err != nil {
		t.Fatal(err)
	}
	got, ok := s.byID("abc")
	if !ok {
		t.Fatal("byID should find resurrected row")
	}
	if got.DeletedAt != nil {
		t.Error("DeletedAt should be cleared after resurrect")
	}
	if got.BodyHash != hashBytes([]byte("world")) {
		t.Errorf("BodyHash should reflect resurrect body, got %q", got.BodyHash)
	}
}

func TestStoreMigrationReapsExpiredTombstones(t *testing.T) {
	dir := t.TempDir()
	// Seed sessions.json with a stale tombstone (older than tombstoneTTL).
	stale := time.Now().Add(-tombstoneTTL - time.Hour)
	json := `{"sessions":[{"id":"old","title":"x","deletedAt":"` + stale.Format(time.RFC3339Nano) + `","updatedAt":"` + stale.Format(time.RFC3339Nano) + `"}]}`
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(json), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Store{file: filepath.Join(dir, "sessions.json"), sessRoot: dir}
	if err := s.load(); err != nil {
		t.Fatal(err)
	}
	if got := len(s.listAll()); got != 0 {
		t.Errorf("expired tombstone should be reaped on load; got %d rows", got)
	}
}

func TestStoreUpdateBumpsUpdatedAt(t *testing.T) {
	s, root := newTestStore(t)
	path := filepath.Join(root, "plan.md")
	writeFile(t, path, "hello")
	_ = s.add(Session{ID: "abc", Title: "Plan", Path: path, Created: time.Now()})
	first, _ := s.byID("abc")
	time.Sleep(2 * time.Millisecond)
	if err := s.update("abc", func(sess *Session) { sess.Title = "Plan v2" }); err != nil {
		t.Fatal(err)
	}
	second, _ := s.byID("abc")
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Error("update() should bump UpdatedAt past the previous value")
	}
}

func TestStoreUpdateNoTouchPreservesUpdatedAt(t *testing.T) {
	s, root := newTestStore(t)
	path := filepath.Join(root, "plan.md")
	writeFile(t, path, "hello")
	_ = s.add(Session{ID: "abc", Title: "Plan", Path: path, Created: time.Now()})
	first, _ := s.byID("abc")
	time.Sleep(2 * time.Millisecond)
	if err := s.updateNoTouch("abc", func(sess *Session) { sess.Title = "Plan v2" }); err != nil {
		t.Fatal(err)
	}
	second, _ := s.byID("abc")
	if !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Error("updateNoTouch() must not bump UpdatedAt")
	}
}

func TestSuppressorConsumeMatchesHash(t *testing.T) {
	s := newSyncSuppressor()
	s.suppressWrite("/tmp/x.md", "abc")

	if !s.consume("/tmp/x.md", "abc") {
		t.Error("consume should match identical path+hash")
	}
	if s.consume("/tmp/x.md", "abc") {
		t.Error("consume should be one-shot")
	}

	s.suppressWrite("/tmp/x.md", "abc")
	if s.consume("/tmp/x.md", "different") {
		t.Error("consume should not match a different hash")
	}
}

func TestSuppressorRemove(t *testing.T) {
	s := newSyncSuppressor()
	s.suppressRemove("/tmp/x.md")
	if !s.consumeRemove("/tmp/x.md") {
		t.Error("consumeRemove should match a recent suppressRemove")
	}
	if s.consumeRemove("/tmp/x.md") {
		t.Error("consumeRemove should be one-shot")
	}
}
