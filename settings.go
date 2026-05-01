package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Settings is the user-editable configuration loaded from data/settings.json.
// Only sessions_folder is honoured today; the file is created with defaults
// on first launch so the user has something to point at an Obsidian vault
// (or any other markdown directory) without hand-crafting JSON.
type Settings struct {
	SessionsFolder string     `json:"sessions_folder"`
	Sync           syncConfig `json:"sync,omitempty"`

	mu      sync.Mutex
	file    string
	rootAbs string
}

// syncCfg returns a snapshot of the current sync configuration. The
// engine and HTTP handlers use this to read config without holding the
// settings mutex past the call.
func (s *Settings) syncCfg() syncConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Sync
}

// updateSync atomically replaces the sync configuration block and
// persists the file. Callers pass a mutator so partial updates (e.g.
// "save URL + key, leave email alone") compose without read-modify-write
// races against the disk.
func (s *Settings) updateSync(fn func(*syncConfig)) error {
	s.mu.Lock()
	fn(&s.Sync)
	s.mu.Unlock()
	return s.save()
}

func loadSettings(rootAbs, file string) *Settings {
	s := &Settings{file: file, rootAbs: rootAbs}
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			if werr := s.save(); werr != nil {
				log.Printf("settings write defaults: %v", werr)
			}
		} else {
			log.Printf("settings read: %v (using defaults)", err)
		}
		return s
	}
	if err := json.Unmarshal(data, s); err != nil {
		log.Printf("settings parse: %v (using defaults)", err)
	}
	return s
}

func (s *Settings) save() error {
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

// resolvedSessionsFolder returns the absolute sessions folder. Empty value
// falls back to <rootAbs>/sessions (the historical default); a relative path
// resolves against rootAbs.
func (s *Settings) resolvedSessionsFolder() string {
	s.mu.Lock()
	folder := strings.TrimSpace(s.SessionsFolder)
	s.mu.Unlock()
	if folder == "" {
		return filepath.Join(s.rootAbs, "sessions")
	}
	if !filepath.IsAbs(folder) {
		return filepath.Join(s.rootAbs, folder)
	}
	return filepath.Clean(folder)
}
