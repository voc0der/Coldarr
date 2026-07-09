// Package orphans finds folders sitting on a configured tier path that no
// longer correspond to anything Radarr, Sonarr, or Jellyfin still tracks -
// a movie deleted from Radarr whose folder was never cleaned up, a failed
// or partial import, a leftover from a prior reorganization. "Known" means
// known to any configured service, not just the two Arr apps: a Jellyfin
// library can legitimately hold content (home videos, directly-added
// media) that never touches Radarr/Sonarr, so that alone doesn't make it
// an orphan.
//
// The scan walks every configured tier's paths on disk, which can be slow
// on a large or slow (archival, near-full) cold tier - exactly the kind of
// call that must never run live on a page view (see internal/cutoffcache's
// package doc for the outage that taught this lesson). It's refreshed only
// via the "Scan for Orphaned Storage" scheduled task or its manual
// "Scan now" button, never from a GET request.
package orphans

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/jellyfin"
	"github.com/vocoder/coldarr/internal/model"
)

// Candidate is one folder found on a tier path that doesn't correspond to
// anything any configured service still tracks.
type Candidate struct {
	Path      string         `json:"path"`
	TierName  string         `json:"tier_name"`
	TierRole  model.TierRole `json:"tier_role"`
	SizeBytes int64          `json:"size_bytes"`
}

// Snapshot is a point-in-time result of the last scan.
type Snapshot struct {
	Candidates []Candidate `json:"candidates"`
	// ScannedAt is the zero Time until Refresh has succeeded at least
	// once - callers use this to tell "never scanned" from "scanned, and
	// found nothing."
	ScannedAt time.Time `json:"scanned_at"`
	// TierWritable/TierWriteError record, per tier path, whether a
	// write-probe succeeded as of ScannedAt, and why not if it didn't -
	// used to grey out deletion for a path rather than let an operator
	// click Delete and hit a raw error. Not a substitute for re-probing
	// immediately before an actual delete, since a scan can be hours old.
	TierWritable   map[string]bool   `json:"tier_writable"`
	TierWriteError map[string]string `json:"tier_write_error,omitempty"`
	// Warnings notes any configured tier path that was skipped entirely
	// (e.g. failed its existence/mount check), mirroring the planner's
	// own "skip, don't assume" treatment of an unusable path.
	Warnings []string `json:"warnings,omitempty"`
}

// Store is a file-backed cache of the current Snapshot, safe for
// concurrent use within one process.
type Store struct {
	path string

	mu   sync.RWMutex
	snap Snapshot
}

// Load reads the cache file at path, starting with an empty (never
// scanned) Snapshot if it does not yet exist.
func Load(path string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading orphan scan cache %s: %w", path, err)
	}
	if len(data) == 0 {
		return s, nil
	}

	if err := json.Unmarshal(data, &s.snap); err != nil {
		return nil, fmt.Errorf("parsing orphan scan cache %s: %w", path, err)
	}
	return s, nil
}

// Get returns a copy of the current snapshot - a zero-value ScannedAt
// means it has never been successfully scanned. Nil-safe: returns an
// empty Snapshot for a nil *Store.
func (s *Store) Get() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

// Refresh walks every configured tier's paths and reports every folder
// that doesn't correspond to a path any configured service (Radarr,
// Sonarr, Jellyfin) still tracks. Skips whichever client is nil (that app
// isn't configured) when building the known-path set, matching
// cutoffcache/linkcache's pattern. A tier path failing its existence/mount
// check is recorded as a warning and skipped, not an error; any other
// failure (a service unreachable, a directory unreadable partway through
// a walk) fails the whole refresh and leaves the stored snapshot
// unchanged, matching cutoffcache/linkcache's all-or-nothing behavior.
func (s *Store) Refresh(radarr *arrapi.RadarrClient, sonarr *arrapi.SonarrClient, jf *jellyfin.Client, tiers []model.Tier) error {
	known, err := knownPaths(radarr, sonarr, jf)
	if err != nil {
		return err
	}
	ancestors := ancestorDirs(known)

	next := Snapshot{
		ScannedAt:      time.Now(),
		TierWritable:   map[string]bool{},
		TierWriteError: map[string]string{},
	}

	for _, tier := range tiers {
		for _, path := range tier.Paths {
			if err := diskusage.CheckPath(path, tier.RequireMount); err != nil {
				next.Warnings = append(next.Warnings, fmt.Sprintf("skipping %s: %v", path, err))
				continue
			}

			candidates, err := scanPath(path, tier, known, ancestors)
			if err != nil {
				return fmt.Errorf("scanning %s: %w", path, err)
			}
			next.Candidates = append(next.Candidates, candidates...)

			writable, reason := probeWritable(path)
			next.TierWritable[path] = writable
			if reason != "" {
				next.TierWriteError[path] = reason
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = next
	return s.save()
}

// knownPaths returns the cleaned filesystem path of every item any
// configured service currently tracks. Radarr/Sonarr's plain movie/series
// list (not /wanted/cutoff) is the same fast call Dashboard/Plan already
// make every load - only the filesystem walk in Refresh is the expensive
// part this package exists to keep off the request path.
func knownPaths(radarr *arrapi.RadarrClient, sonarr *arrapi.SonarrClient, jf *jellyfin.Client) (map[string]bool, error) {
	known := map[string]bool{}

	if radarr != nil {
		movies, err := radarr.FetchMovies()
		if err != nil {
			return nil, fmt.Errorf("fetching radarr movies: %w", err)
		}
		for _, m := range movies {
			known[filepath.Clean(m.Path)] = true
		}
	}

	if sonarr != nil {
		series, err := sonarr.FetchSeries()
		if err != nil {
			return nil, fmt.Errorf("fetching sonarr series: %w", err)
		}
		for _, sr := range series {
			known[filepath.Clean(sr.Path)] = true
		}
	}

	if jf != nil {
		ids, err := jf.LibraryItemIDs()
		if err != nil {
			return nil, fmt.Errorf("fetching jellyfin library item paths: %w", err)
		}
		for path := range ids {
			known[filepath.Clean(path)] = true
		}
	}

	return known, nil
}

// ancestorDirs returns every directory that is a strict ancestor of some
// known path - organizational folders (e.g. a tier's /TV, or a genre
// subfolder above the actual show folder) that a scan should keep
// descending into rather than flag as orphaned themselves.
func ancestorDirs(known map[string]bool) map[string]bool {
	ancestors := map[string]bool{}
	for path := range known {
		for dir := filepath.Dir(path); dir != "." && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
			if ancestors[dir] {
				break // this dir and everything above it was already registered by an earlier path
			}
			ancestors[dir] = true
		}
	}
	return ancestors
}

// scanPath walks the directory tree rooted at path (one of tier's
// configured paths), pruning at any directory matching a known item's own
// path (its internal structure - seasons, extras, subs - is that item's
// own business, not this scan's concern) and descending through any
// organizational ancestor directory. Anything else is the root of an
// orphaned subtree - reported once, at its root, not per file inside it.
func scanPath(path string, tier model.Tier, known, ancestors map[string]bool) ([]Candidate, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var candidates []Candidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue // loose files directly under a tier path aren't item folders; not this scan's concern
		}
		child := filepath.Join(path, entry.Name())

		switch {
		case known[child]:
			continue // a tracked item's own folder - don't descend, don't flag
		case ancestors[child]:
			sub, err := scanPath(child, tier, known, ancestors)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, sub...)
		default:
			size, err := dirSize(child)
			if err != nil {
				return nil, fmt.Errorf("sizing %s: %w", child, err)
			}
			candidates = append(candidates, Candidate{
				Path:      child,
				TierName:  tier.Name,
				TierRole:  tier.Role,
				SizeBytes: size,
			})
		}
	}
	return candidates, nil
}

// dirSize returns the total size of every regular file under path.
func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// probeWritable attempts to create and remove a small marker file in dir,
// reporting whether the write actually succeeded - not just what the
// mount's declared flags claim, since those can lie (a mount can report
// rw while the underlying export/device rejects writes) - and why not if
// it failed, so the reason (e.g. "read-only file system", "permission
// denied") can be shown directly rather than only surfacing if someone
// later tries to delete something there.
func probeWritable(dir string) (ok bool, reason string) {
	f, err := os.CreateTemp(dir, ".coldarr-writetest-")
	if err != nil {
		return false, err.Error()
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// save must be called with mu held.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding orphan scan cache: %w", err)
	}

	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating orphan scan cache directory %s: %w", dir, err)
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
