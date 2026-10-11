// Package trackercache remembers which trackers each qBittorrent download
// announces to - what its seeding requirement depends on (see
// internal/torrents). qBittorrent only answers that one download per
// request, so for a few thousand downloads it's a few thousand requests:
// exactly the kind of call internal/cutoffcache's package doc warns must
// never run on a page view. It's refreshed only by the "Scan Torrent
// Trackers" scheduled task or its "Scan now" button, and each download is
// looked up once (then again only every refreshAfter, in case its trackers
// were edited), not on every scan.
//
// Only tracker hosts are kept, never announce URLs: those usually carry
// the account's passkey.
package trackercache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/qbit"
)

// refreshAfter is how long a download's trackers are trusted before being
// looked up again - they rarely change, but a tracker moving to a new
// domain changes them for every download it has.
const refreshAfter = 30 * 24 * time.Hour

// lookupConcurrency bounds how many tracker lookups run at once against
// qBittorrent.
const lookupConcurrency = 4

// Entry is one download's trackers.
type Entry struct {
	Hosts     []string  `json:"hosts"`
	CheckedAt time.Time `json:"checked_at"`
}

// Snapshot is the cache's contents.
type Snapshot struct {
	// Downloads maps a (lower-case) hash to its trackers. A download with
	// no entry hasn't been looked up yet, which is not the same as having
	// no trackers.
	Downloads map[string]Entry `json:"downloads"`
	// Seen is every tracker host any scan has found, with when it was
	// last seen. It's never pruned: the Seeding rules picker keeps
	// offering a tracker after the downloads that used it are gone.
	Seen map[string]time.Time `json:"seen"`
	// RefreshedAt is the zero Time until a scan has completed.
	RefreshedAt time.Time `json:"refreshed_at"`
}

// Hosts returns the hosts cached for hash, and whether it has been looked
// up at all.
func (s Snapshot) Hosts(hash string) ([]string, bool) {
	e, ok := s.Downloads[hash]
	return e.Hosts, ok
}

// SeenHosts returns every host ever seen, sorted.
func (s Snapshot) SeenHosts() []string {
	out := make([]string, 0, len(s.Seen))
	for h := range s.Seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// Store is a file-backed cache, safe for concurrent use within one
// process. A nil *Store is empty.
type Store struct {
	path string

	mu   sync.RWMutex
	snap Snapshot
}

// Load reads the cache at path, starting empty if it doesn't exist.
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading tracker cache %s: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.snap); err != nil {
		return nil, fmt.Errorf("parsing tracker cache %s: %w", path, err)
	}
	return s, nil
}

// Get returns a copy of the current snapshot.
func (s *Store) Get() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Snapshot{RefreshedAt: s.snap.RefreshedAt, Downloads: make(map[string]Entry, len(s.snap.Downloads)), Seen: make(map[string]time.Time, len(s.snap.Seen))}
	for k, v := range s.snap.Downloads {
		out.Downloads[k] = v
	}
	for k, v := range s.snap.Seen {
		out.Seen[k] = v
	}
	return out
}

// Refresh looks up the trackers of every download in list that isn't
// cached yet, or was cached more than refreshAfter ago, and forgets
// downloads qBittorrent no longer has. Hosts found are added to Seen.
//
// It saves what it resolved even when some lookups fail - a scan of
// thousands of downloads shouldn't be thrown away over one hiccup - and
// returns the first failure. A download removed between the list and its
// lookup is skipped.
func (s *Store) Refresh(c *qbit.Client, list []qbit.Torrent, now time.Time) (looked int, err error) {
	current := s.Get()

	var todo []string
	keep := make(map[string]bool, len(list))
	for _, t := range list {
		keep[t.Hash] = true
		if e, ok := current.Downloads[t.Hash]; !ok || now.Sub(e.CheckedAt) > refreshAfter {
			todo = append(todo, t.Hash)
		}
	}

	type result struct {
		hash  string
		hosts []string
		err   error
	}
	results := make([]result, len(todo))
	sem := make(chan struct{}, lookupConcurrency)
	var wg sync.WaitGroup
	for i, hash := range todo {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			hosts, err := c.TrackerHosts(hash)
			results[i] = result{hash: hash, hosts: hosts, err: err}
		}()
	}
	wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap.Downloads == nil {
		s.snap.Downloads = map[string]Entry{}
	}
	if s.snap.Seen == nil {
		s.snap.Seen = map[string]time.Time{}
	}
	var firstErr error
	for _, r := range results {
		switch {
		case errors.Is(r.err, qbit.ErrNotFound):
			continue
		case r.err != nil:
			if firstErr == nil {
				firstErr = fmt.Errorf("looking up the trackers of download %s: %w", r.hash, r.err)
			}
			continue
		}
		s.snap.Downloads[r.hash] = Entry{Hosts: r.hosts, CheckedAt: now}
		looked++
	}
	for hash := range s.snap.Downloads {
		if !keep[hash] {
			delete(s.snap.Downloads, hash)
		}
	}
	// Seen comes from every cached download, plus each one's currently
	// working tracker - free in the listing, so the picker has hosts to
	// offer even before a scan has looked up a single download.
	for _, e := range s.snap.Downloads {
		for _, h := range e.Hosts {
			s.snap.Seen[h] = now
		}
	}
	for _, t := range list {
		if t.TrackerHost != "" {
			s.snap.Seen[t.TrackerHost] = now
		}
	}
	if firstErr == nil {
		s.snap.RefreshedAt = now
	}
	if err := s.saveLocked(); err != nil {
		return looked, err
	}
	return looked, firstErr
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding tracker cache: %w", err)
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating tracker cache directory %s: %w", dir, err)
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
