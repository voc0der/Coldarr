// Package orphans finds folders sitting on a configured tier path that no
// longer correspond to anything Radarr, Sonarr, Lidarr, Jellyfin or
// qBittorrent still tracks - a movie deleted from Radarr whose folder was
// never cleaned up, a failed or partial import, a leftover from a prior
// reorganization, a download removed from qBittorrent without its files.
// "Known" means known to any configured service, not just the two Arr
// apps: a Jellyfin library can legitimately hold content (home videos,
// directly-added media) that never touches Radarr/Sonarr, so that alone
// doesn't make it an orphan.
//
// Download folders (downloads tiers) are compared against qBittorrent entry
// by entry, loose files included, since a single-file download is a file
// sitting directly in the folder. They're the one place Coldarr can delete
// what it finds, and only when someone asks it to, for a tier marked as
// not read-only (see DeleteDownload).
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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/jellyfin"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/qbit"
)

// Candidate is one folder found on a tier path that doesn't correspond to
// anything any configured service still tracks.
type Candidate struct {
	Path      string         `json:"path"`
	TierName  string         `json:"tier_name"`
	TierRole  model.TierRole `json:"tier_role"`
	SizeBytes int64          `json:"size_bytes"`
	// Domain is which kind of tier it was found on - empty, meaning
	// library, in a cache written before downloads tiers existed.
	Domain model.TierDomain `json:"domain,omitempty"`
	// File marks a loose file rather than a folder - only ever found in a
	// download folder.
	File bool `json:"file,omitempty"`
}

// IsDownloads reports whether the candidate is in a download folder.
func (c Candidate) IsDownloads() bool {
	return c.Domain == model.DomainDownloads
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

// Sources are the services whose content counts as known. Any of them may
// be nil - that app isn't configured.
type Sources struct {
	Radarr   *arrapi.RadarrClient
	Sonarr   *arrapi.SonarrClient
	Lidarr   *arrapi.LidarrClient
	Jellyfin *jellyfin.Client
	Qbit     *qbit.Client
}

// Refresh walks every configured tier's paths and reports every folder
// that doesn't correspond to a path any configured service still tracks -
// and, in download folders, every loose file too. Skips whichever client
// is nil (that app isn't configured) when building the known-path set,
// matching cutoffcache/linkcache's pattern. Download folders are skipped,
// with a warning, when qBittorrent isn't connected: nothing else accounts
// for downloads, so every one of them would look orphaned. A tier path
// failing its existence/mount check is recorded as a warning and skipped,
// not an error; any other failure (a service unreachable, a directory
// unreadable partway through a walk) fails the whole refresh and leaves
// the stored snapshot unchanged, matching cutoffcache/linkcache's
// all-or-nothing behavior.
func (s *Store) Refresh(src Sources, tiers []model.Tier) error {
	known, err := knownPaths(src)
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
			if tier.IsDownloads() && src.Qbit == nil {
				next.Warnings = append(next.Warnings, fmt.Sprintf("skipping %s: qBittorrent isn't connected, so there's nothing to tell its downloads from orphans", path))
				continue
			}
			if err := diskusage.CheckPath(path, tier.RequireMount); err != nil {
				next.Warnings = append(next.Warnings, fmt.Sprintf("skipping %s: %v", path, err))
				continue
			}

			var candidates []Candidate
			if tier.IsDownloads() {
				candidates, err = scanDownloads(path, tier, known, ancestors)
			} else {
				candidates, err = scanPath(path, tier, known, ancestors)
			}
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
func knownPaths(src Sources) (map[string]bool, error) {
	known := map[string]bool{}
	radarr, sonarr, jf := src.Radarr, src.Sonarr, src.Jellyfin

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

	if src.Lidarr != nil {
		paths, err := src.Lidarr.ArtistPaths()
		if err != nil {
			return nil, fmt.Errorf("fetching lidarr artists: %w", err)
		}
		for _, p := range paths {
			known[filepath.Clean(p)] = true
		}
	}

	if src.Qbit != nil {
		if err := addDownloads(src.Qbit, known); err != nil {
			return nil, err
		}
	}

	return known, nil
}

// addDownloads adds what every qBittorrent download occupies to known: its
// content path - the root folder of a multi-file download, or the file of
// a single-file one. A download saved with no folder of its own
// (qBittorrent's "Don't create subfolder" layout) reports its save folder
// as its content, which would claim everything else in that folder too, so
// for those the top-level entries of its actual files are added instead.
func addDownloads(c *qbit.Client, known map[string]bool) error {
	list, err := c.Torrents()
	if err != nil {
		return fmt.Errorf("fetching qbittorrent downloads: %w", err)
	}
	for _, t := range list {
		if t.ContentPath == "" {
			continue
		}
		content, save := filepath.Clean(t.ContentPath), filepath.Clean(t.SavePath)
		if content != save {
			known[content] = true
			continue
		}
		names, err := c.FileNames(t.Hash)
		if errors.Is(err, qbit.ErrNotFound) {
			continue // removed since it was listed
		}
		if err != nil {
			return fmt.Errorf("fetching the files of qbittorrent download %q: %w", t.Name, err)
		}
		for _, name := range names {
			top := strings.SplitN(filepath.ToSlash(name), "/", 2)[0]
			if top != "" && top != "." && top != ".." {
				known[filepath.Join(save, top)] = true
			}
		}
	}
	return nil
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
				Domain:    tier.Domain,
			})
		}
	}
	return candidates, nil
}

// incompleteSuffix is what qBittorrent appends to a file it hasn't
// finished downloading, when set to - the file on disk is name.!qB while
// qBittorrent still reports it as name.
const incompleteSuffix = ".!qB"

// scanDownloads walks a download folder the way scanPath walks a library
// folder, except that every entry is judged, files included: a single-file
// download is just a file in the folder, so a leftover one is too.
// Symlinks are judged as entries, never followed.
func scanDownloads(path string, tier model.Tier, known, ancestors map[string]bool) ([]Candidate, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var candidates []Candidate
	for _, entry := range entries {
		child := filepath.Join(path, entry.Name())
		switch {
		case known[child]:
			continue
		case strings.HasSuffix(entry.Name(), incompleteSuffix) && known[strings.TrimSuffix(child, incompleteSuffix)]:
			continue
		case entry.IsDir() && ancestors[child]:
			sub, err := scanDownloads(child, tier, known, ancestors)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, sub...)
			continue
		}

		c := Candidate{Path: child, TierName: tier.Name, TierRole: tier.Role, Domain: model.DomainDownloads, File: !entry.IsDir()}
		if entry.IsDir() {
			if c.SizeBytes, err = dirSize(child); err != nil {
				return nil, fmt.Errorf("sizing %s: %w", child, err)
			}
		} else {
			info, err := entry.Info()
			if err != nil {
				return nil, fmt.Errorf("sizing %s: %w", child, err)
			}
			c.SizeBytes = info.Size()
		}
		candidates = append(candidates, c)
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

// DeleteDownload deletes one entry the last scan found in a download
// folder - the only thing Coldarr ever deletes from disk itself - and drops
// it from the snapshot. Every guard is re-checked against the present,
// since a scan can be hours old:
//
//   - path must be exactly an entry the last scan listed in a download
//     folder - never an arbitrary path from a form;
//   - its tier must still be a downloads tier marked as not read-only,
//     with path strictly inside one of its paths;
//   - that tier path must pass its storage check, so nothing is deleted
//     from an empty mountpoint left by a missing drive (or worse, from
//     whatever is mounted there instead);
//   - and, asked afresh, no configured service may know path, or anything
//     inside it - a download added since the scan may be using it.
func (s *Store) DeleteDownload(path string, src Sources, tiers []model.Tier) (Candidate, error) {
	if src.Qbit == nil {
		return Candidate{}, fmt.Errorf("qBittorrent isn't connected, so there's no telling what in its download folders is orphaned")
	}
	path = filepath.Clean(path)

	var found *Candidate
	for _, c := range s.Get().Candidates {
		if c.IsDownloads() && c.Path == path {
			found = &c
			break
		}
	}
	if found == nil {
		return Candidate{}, fmt.Errorf("%s isn't on the list from the last scan of the download folders - scan again", path)
	}

	var tier *model.Tier
	for i := range tiers {
		if tiers[i].Name == found.TierName {
			tier = &tiers[i]
			break
		}
	}
	if tier == nil || !tier.IsDownloads() {
		return Candidate{}, fmt.Errorf("the tier %s was found in is no longer a downloads tier", path)
	}
	if !tier.AllowDeletes {
		return Candidate{}, fmt.Errorf("tier %q is read-only - mark it as not read-only under Settings > Storage tiers to delete from it", tier.Name)
	}
	root := ""
	for _, p := range tier.Paths {
		if p = filepath.Clean(p); strings.HasPrefix(path, p+string(filepath.Separator)) {
			root = p
			break
		}
	}
	if root == "" {
		return Candidate{}, fmt.Errorf("%s isn't inside any of tier %q's paths", path, tier.Name)
	}
	if err := diskusage.CheckPath(root, tier.RequireMount); err != nil {
		return Candidate{}, fmt.Errorf("refusing to delete from %s: %w", root, err)
	}

	known, err := knownPaths(src)
	if err != nil {
		return Candidate{}, fmt.Errorf("checking %s is still orphaned: %w", path, err)
	}
	for k := range known {
		if k == path || strings.HasPrefix(k, path+string(filepath.Separator)) || strings.HasPrefix(path, k+string(filepath.Separator)) {
			return Candidate{}, fmt.Errorf("%s is in use now (%s) - nothing was deleted", path, k)
		}
	}
	if _, err := os.Lstat(path); err != nil {
		return Candidate{}, fmt.Errorf("checking %s: %w", path, err)
	}

	if err := os.RemoveAll(path); err != nil {
		return Candidate{}, fmt.Errorf("deleting %s: %w", path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.snap.Candidates[:0:0]
	for _, c := range s.snap.Candidates {
		if c.Path != path {
			kept = append(kept, c)
		}
	}
	s.snap.Candidates = kept
	return *found, s.save()
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
