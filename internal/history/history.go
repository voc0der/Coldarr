// Package history persists a log of moves Coldarr has performed, so the
// planner can enforce a cooldown and avoid moving the same item over and
// over.
package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/model"
)

type Record struct {
	ArrApp    string    `json:"arr_app"`
	ItemID    int       `json:"item_id"`
	Title     string    `json:"title"`
	FromTier  string    `json:"from_tier"`
	FromPath  string    `json:"from_path"`
	ToTier    string    `json:"to_tier"`
	ToPath    string    `json:"to_path"`
	SizeBytes int64     `json:"size_bytes"`
	MovedAt   time.Time `json:"moved_at"`
}

// Store is an append-only, file-backed log of past moves. It is safe for
// concurrent use within one process (the mover runs multiple volumes'
// moves in parallel, each recording history as it completes) but not
// across separate processes; the CLI and the web GUI each run their own
// apply at a time thanks to mover.Lock.
type Store struct {
	path string

	mu      sync.Mutex
	records []Record
}

// Load reads the history file at path, creating an empty store if it does
// not yet exist.
func Load(path string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading history %s: %w", path, err)
	}

	if len(data) == 0 {
		return s, nil
	}

	if err := json.Unmarshal(data, &s.records); err != nil {
		return nil, fmt.Errorf("parsing history %s: %w", path, err)
	}
	return s, nil
}

// Append records a completed move and immediately persists the store to
// disk, so a crash mid-run doesn't lose already-completed moves from the
// cooldown ledger.
func (s *Store) Append(rec Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
	return s.save()
}

// save must be called with mu held.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding history: %w", err)
	}

	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating history directory %s: %w", dir, err)
		}
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing history %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("saving history %s: %w", s.path, err)
	}
	return nil
}

// LastMoved returns the most recent time the given item was moved, if ever.
func (s *Store) LastMoved(key model.Key) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var latest time.Time
	found := false
	for _, r := range s.records {
		if r.ArrApp == key.ArrApp && r.ItemID == key.ID {
			if !found || r.MovedAt.After(latest) {
				latest = r.MovedAt
				found = true
			}
		}
	}
	return latest, found
}

// InCooldown reports whether the item was moved within the last cooldown
// window as of now.
func (s *Store) InCooldown(key model.Key, cooldown time.Duration, now time.Time) bool {
	last, ok := s.LastMoved(key)
	if !ok {
		return false
	}
	return now.Sub(last) < cooldown
}

// All returns every recorded move, most recent first, for the audit/
// history view. The returned slice is a copy - callers cannot mutate the
// store's internal state through it.
func (s *Store) All() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Record, len(s.records))
	copy(out, s.records)
	sort.Slice(out, func(i, j int) bool { return out[i].MovedAt.After(out[j].MovedAt) })
	return out
}
