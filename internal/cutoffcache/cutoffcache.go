// Package cutoffcache persists which Radarr movies / Sonarr series
// currently have a file that doesn't meet its quality profile's upgrade
// cutoff. Coldarr's scoring uses this to keep such items on hot storage
// (see internal/scoring) - the owning Arr app will keep searching for a
// better release, so the folder isn't settled yet.
//
// This is deliberately never fetched live on a Dashboard/Plan page view:
// Radarr/Sonarr's own /wanted/cutoff endpoint is known to be slow on
// real-world libraries (Sonarr's especially, since it's evaluated per
// episode, not per series), and doing that on every page load turned a
// v0.18.0 upgrade into an outage for at least one real library. Instead
// this is refreshed on a schedule (see the "Scan Quality Cutoffs"
// scheduled task) or on demand via its "Scan now" button, exactly like
// internal/linkcache's Links-column reference data.
package cutoffcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
)

// Snapshot is a point-in-time copy of which items have an unmet quality
// cutoff, without hitting Radarr/Sonarr live.
type Snapshot struct {
	RadarrUnmetIDs map[int]bool `json:"radarr_unmet_ids"`
	SonarrUnmetIDs map[int]bool `json:"sonarr_unmet_ids"`
	// RefreshedAt is the zero Time until Refresh has succeeded at least
	// once - callers use this to tell "nothing known yet" (e.g. a fresh
	// install, or an upgrade before its first scan) from "refreshed, but
	// nothing is cutoff-unmet."
	RefreshedAt time.Time `json:"refreshed_at"`
}

// Store is a file-backed cache of the current Snapshot, safe for
// concurrent use within one process. A nil *Store behaves like an empty,
// never-refreshed Snapshot on Get - callers never need to check "is this
// configured" themselves.
type Store struct {
	path string

	mu   sync.RWMutex
	snap Snapshot
}

// Load reads the cache file at path, starting with an empty (never
// refreshed) Snapshot if it does not yet exist.
func Load(path string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading quality-cutoff cache %s: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}

	if err := json.Unmarshal(data, &s.snap); err != nil {
		return nil, fmt.Errorf("parsing quality-cutoff cache %s: %w", path, err)
	}
	return s, nil
}

// Get returns a copy of the current snapshot - a zero-value RefreshedAt
// means it has never been successfully refreshed.
func (s *Store) Get() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

// Refresh live-fetches Radarr/Sonarr's wanted/cutoff lists (skipping
// whichever client is nil, i.e. that app isn't configured) and atomically
// replaces and persists the snapshot. This is the only place Coldarr ever
// calls arrapi's CutoffUnmetMovieIDs/CutoffUnmetSeriesIDs - always from a
// scheduled tick or an explicit "Scan now" click, never from a page
// request, so however long a large library's scan genuinely takes never
// blocks anyone looking at the Dashboard or Plan page.
func (s *Store) Refresh(radarr *arrapi.RadarrClient, sonarr *arrapi.SonarrClient) error {
	next := Snapshot{RefreshedAt: time.Now()}

	if radarr != nil {
		ids, err := radarr.CutoffUnmetMovieIDs()
		if err != nil {
			return fmt.Errorf("fetching radarr cutoff-unmet movie IDs: %w", err)
		}
		next.RadarrUnmetIDs = ids
	}

	if sonarr != nil {
		ids, err := sonarr.CutoffUnmetSeriesIDs()
		if err != nil {
			return fmt.Errorf("fetching sonarr cutoff-unmet series IDs: %w", err)
		}
		next.SonarrUnmetIDs = ids
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = next
	return s.save()
}

// save must be called with mu held.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding quality-cutoff cache: %w", err)
	}

	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating quality-cutoff cache directory %s: %w", dir, err)
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
