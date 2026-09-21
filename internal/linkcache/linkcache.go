// Package linkcache persists the Radarr/Sonarr titleSlug and item-folder
// and Jellyfin item-ID/server-ID lookups the web GUI's Links column needs
// to build a deep link into each app. This is reference data that rarely
// changes - an item's folder only when it moves - so it's refreshed on a
// schedule (see the "Refresh Links cache" scheduled task) rather than live
// on every Plan/History page view.
package linkcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/jellyfin"
)

// Snapshot is a point-in-time copy of everything the Links column needs
// to build hrefs, without hitting Radarr/Sonarr/Jellyfin live.
type Snapshot struct {
	RadarrTitleSlugByID map[int]string `json:"radarr_title_slug_by_id"`
	SonarrTitleSlugByID map[int]string `json:"sonarr_title_slug_by_id"`
	// RadarrPathByID/SonarrPathByID are each item's current folder - what
	// JellyfinPathToID is keyed by. History records only the tier roots a
	// move went between, so this is how a History row finds its Jellyfin
	// item, wherever the item lives now. Absent from a cache written before
	// they existed, until its next refresh; the link is then just omitted.
	RadarrPathByID   map[int]string    `json:"radarr_path_by_id"`
	SonarrPathByID   map[int]string    `json:"sonarr_path_by_id"`
	JellyfinPathToID map[string]string `json:"jellyfin_path_to_id"`
	JellyfinServerID string            `json:"jellyfin_server_id"`
	// RefreshedAt is the zero Time until Refresh has succeeded at least
	// once - callers use this to tell "nothing known yet" (e.g. a fresh
	// install before its first scheduled refresh) from "refreshed, but
	// nothing to report."
	RefreshedAt time.Time `json:"refreshed_at"`
}

// Store is a file-backed cache of the current Snapshot, safe for
// concurrent use within one process.
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
		return nil, fmt.Errorf("reading link cache %s: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}

	if err := json.Unmarshal(data, &s.snap); err != nil {
		return nil, fmt.Errorf("parsing link cache %s: %w", path, err)
	}
	return s, nil
}

// Get returns a copy of the current snapshot - a zero-value RefreshedAt
// means it has never been successfully refreshed.
func (s *Store) Get() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

// Refresh live-fetches every piece of Links reference data (skipping
// whichever client is nil, i.e. that app isn't configured) and atomically
// replaces and persists the snapshot. Partial data (e.g. Jellyfin
// configured but Radarr not) is normal, not an error - only a genuine
// fetch failure against a configured app fails the refresh.
func (s *Store) Refresh(radarr *arrapi.RadarrClient, sonarr *arrapi.SonarrClient, jf *jellyfin.Client) error {
	next := Snapshot{RefreshedAt: time.Now()}

	if radarr != nil {
		targets, err := radarr.LinkTargets()
		if err != nil {
			return fmt.Errorf("fetching radarr link targets: %w", err)
		}
		next.RadarrTitleSlugByID, next.RadarrPathByID = splitTargets(targets)
	}

	if sonarr != nil {
		targets, err := sonarr.LinkTargets()
		if err != nil {
			return fmt.Errorf("fetching sonarr link targets: %w", err)
		}
		next.SonarrTitleSlugByID, next.SonarrPathByID = splitTargets(targets)
	}

	if jf != nil {
		ids, err := jf.LibraryItemIDs()
		if err != nil {
			return fmt.Errorf("fetching jellyfin library item IDs: %w", err)
		}
		next.JellyfinPathToID = ids

		serverID, err := jf.ServerID()
		if err != nil {
			return fmt.Errorf("fetching jellyfin server ID: %w", err)
		}
		next.JellyfinServerID = serverID
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = next
	return s.save()
}

// splitTargets stores slugs and folders as separate maps, so a cache file
// from before folders were cached still decodes into this Snapshot.
func splitTargets(targets map[int]arrapi.LinkTarget) (slugs, paths map[int]string) {
	slugs = make(map[int]string, len(targets))
	paths = make(map[int]string, len(targets))
	for id, t := range targets {
		slugs[id] = t.TitleSlug
		paths[id] = t.Path
	}
	return slugs, paths
}

// save must be called with mu held.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding link cache: %w", err)
	}

	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating link cache directory %s: %w", dir, err)
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
