// Package importcache remembers which downloads Radarr, Sonarr and Lidarr
// have confirmed importing. Before moving a download off the download
// drive, Coldarr asks the app that grabbed it whether the import happened
// (internal/arrapi's Imported), one request per download - and once an
// app says yes, the answer can't change. Keeping the yeses means a download
// is only ever asked about until it's confirmed, however many page views
// and plans it sits through before it moves. "Not yet" is never kept: that
// can change any minute.
package importcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store is a file-backed set of confirmed imports, safe for concurrent use
// within one process. A nil *Store remembers nothing.
type Store struct {
	path string

	mu sync.Mutex
	// confirmed maps "app|HASH" to when the confirmation was first seen.
	confirmed map[string]time.Time
}

// Load reads the cache file at path, starting empty if it doesn't exist.
func Load(path string) (*Store, error) {
	s := &Store{path: path, confirmed: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading import cache %s: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.confirmed); err != nil {
		return nil, fmt.Errorf("parsing import cache %s: %w", path, err)
	}
	if s.confirmed == nil {
		s.confirmed = map[string]time.Time{}
	}
	return s, nil
}

func key(app, hash string) string {
	return app + "|" + hash
}

// Imported reports whether app has already confirmed importing the download
// with this (upper-cased) hash.
func (s *Store) Imported(app, hash string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.confirmed[key(app, hash)]
	return ok
}

// Record keeps each confirmation not already known, and drops every entry
// whose hash isn't in keep - downloads qBittorrent no longer has, so the
// file doesn't grow forever. It writes only when something changed.
func (s *Store) Record(confirmed []Confirmation, keep map[string]bool, now time.Time) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	changed := false
	for _, c := range confirmed {
		k := key(c.App, c.Hash)
		if _, ok := s.confirmed[k]; !ok {
			s.confirmed[k] = now
			changed = true
		}
	}
	for k := range s.confirmed {
		if !keep[hashOf(k)] {
			delete(s.confirmed, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveLocked()
}

// Confirmation is one app's confirmed import of one download.
type Confirmation struct {
	App  string
	Hash string
}

func hashOf(k string) string {
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == '|' {
			return k[i+1:]
		}
	}
	return k
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.confirmed, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding import cache: %w", err)
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating import cache directory %s: %w", dir, err)
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("saving %s: %w", s.path, err)
	}
	return nil
}
