package main

import (
	"crypto/md5" //nolint:gosec // Jellyfin-style item IDs, not security
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// item is one movie or series, as Radarr/Sonarr (and Jellyfin) see it.
type item struct {
	app         string // "radarr" or "sonarr"
	id          int
	title       string
	year        int
	root        string
	size        int64
	added       time.Time
	profile     string
	monitored   bool
	tags        []string
	status      string
	favorite    bool
	cutoffUnmet bool
	downloading bool
	seasons     int
	lastAired   *time.Time
	movedAt     *time.Time
}

func (it *item) folder() string { return fmt.Sprintf("%s (%d)", it.title, it.year) }
func (it *item) path() string   { return filepath.Join(it.root, it.folder()) }
func (it *item) hasFile() bool  { return it.size > 0 }

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

func (it *item) slug() string {
	return strings.Trim(slugStrip.ReplaceAllString(strings.ToLower(it.title), "-"), "-") + fmt.Sprintf("-%d", it.year)
}

const episodesPerSeason = 8

// fileSpec is one media file inside an item's folder.
type fileSpec struct {
	rel  string
	size int64
}

// files lays out an item's folder the way Radarr/Sonarr name things. Sizes
// always sum to exactly it.size.
func (it *item) files() []fileSpec {
	if !it.hasFile() {
		return nil
	}
	if it.app == "radarr" {
		name := fmt.Sprintf("%s (%d) [%s].mkv", it.title, it.year, qualityFor(it.profile))
		return []fileSpec{{rel: name, size: it.size}}
	}
	seasons := max(it.seasons, 1)
	n := int64(seasons * episodesPerSeason)
	each := it.size / n
	var out []fileSpec
	for s := 1; s <= seasons; s++ {
		for e := 1; e <= episodesPerSeason; e++ {
			out = append(out, fileSpec{
				rel:  fmt.Sprintf("Season %02d/%s - S%02dE%02d [%s].mkv", s, it.title, s, e, qualityFor(it.profile)),
				size: each,
			})
		}
	}
	out[len(out)-1].size += it.size - each*n
	return out
}

func (it *item) episodeFileCount() int {
	if !it.hasFile() {
		return 0
	}
	return max(it.seasons, 1) * episodesPerSeason
}

func qualityFor(profile string) string {
	switch profile {
	case "Ultra-HD":
		return "Remux-2160p"
	case "HD-1080p":
		return "Bluray-1080p"
	case "HD-720p":
		return "Bluray-720p"
	case "SD":
		return "DVD"
	default:
		return "WEBDL-1080p"
	}
}

// jellyfinID mimics Jellyfin's path-derived item IDs: a moved item comes
// back under a new ID, just as it does in real Jellyfin.
func jellyfinID(path string) string {
	sum := md5.Sum([]byte(path)) //nolint:gosec // see import
	return hex.EncodeToString(sum[:])
}

type command struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type library struct {
	mu       sync.Mutex
	movies   []*item
	series   []*item
	tags     []string // index+1 is the tag's ID
	profiles []string // index+1 is the profile's ID
	commands map[string][]*command
	nextCmd  int

	sim    simulation
	drives []*drive
}

func newLibrary(fx *fixture, drives []*drive, now time.Time) (*library, error) {
	l := &library{sim: fx.Simulation, drives: drives, commands: map[string][]*command{}}
	tagSet, profileSet := map[string]bool{}, map[string]bool{}

	build := func(app string, specs []itemSpec) ([]*item, error) {
		var out []*item
		for i, s := range specs {
			if s.Title == "" || s.Year == 0 || !filepath.IsAbs(s.Root) {
				return nil, fmt.Errorf("%s item %d (%q) needs a title, a year and an absolute root", app, i+1, s.Title)
			}
			if l.driveOf(s.Root) == nil {
				return nil, fmt.Errorf("%q: root %s is not on any fixture drive", s.Title, s.Root)
			}
			it := &item{
				app:         app,
				id:          i + 1,
				title:       s.Title,
				year:        s.Year,
				root:        filepath.Clean(s.Root),
				size:        int64(s.Size),
				added:       daysAgo(now, s.AddedDaysAgo),
				profile:     s.Profile,
				monitored:   s.Monitored == nil || *s.Monitored,
				tags:        s.Tags,
				status:      s.Status,
				favorite:    s.Favorite,
				cutoffUnmet: s.CutoffUnmet,
				downloading: s.Downloading,
				seasons:     s.Seasons,
			}
			if it.profile == "" {
				it.profile = "HD-1080p"
			}
			if it.status == "" {
				it.status = map[string]string{"radarr": "released", "sonarr": "ended"}[app]
			}
			if s.LastAiredDaysAgo != nil {
				t := daysAgo(now, *s.LastAiredDaysAgo)
				it.lastAired = &t
			}
			if s.MovedDaysAgo != nil {
				t := daysAgo(now, *s.MovedDaysAgo)
				it.movedAt = &t
			}
			for _, t := range it.tags {
				tagSet[t] = true
			}
			profileSet[it.profile] = true
			out = append(out, it)
		}
		return out, nil
	}

	var err error
	if l.movies, err = build("radarr", fx.Movies); err != nil {
		return nil, err
	}
	if l.series, err = build("sonarr", fx.Series); err != nil {
		return nil, err
	}
	for t := range tagSet {
		l.tags = append(l.tags, t)
	}
	sort.Strings(l.tags)
	for p := range profileSet {
		l.profiles = append(l.profiles, p)
	}
	sort.Strings(l.profiles)
	return l, nil
}

func daysAgo(now time.Time, days float64) time.Time {
	return now.Add(-time.Duration(days * float64(24*time.Hour))).Truncate(time.Minute)
}

func (l *library) driveOf(path string) *drive {
	var best *drive
	for _, d := range l.drives {
		if d.contains(path) && (best == nil || len(d.mount) > len(best.mount)) {
			best = d
		}
	}
	return best
}

func (l *library) items(app string) []*item {
	if app == "radarr" {
		return l.movies
	}
	return l.series
}

func (l *library) find(app string, id int) *item {
	for _, it := range l.items(app) {
		if it.id == id {
			return it
		}
	}
	return nil
}

func (l *library) tagID(label string) int       { return slices.Index(l.tags, label) + 1 }
func (l *library) profileID(profile string) int { return slices.Index(l.profiles, profile) + 1 }
func (l *library) allItems() []*item            { return append(slices.Clone(l.movies), l.series...) }

// seed writes every item's files, and the orphan folders, through the
// mounted drives.
func (l *library) seed(orphans []orphanSpec) error {
	for _, it := range l.allItems() {
		for _, f := range it.files() {
			if err := writeSparse(filepath.Join(it.path(), f.rel), f.size); err != nil {
				return err
			}
		}
		if !it.hasFile() {
			if err := os.MkdirAll(it.path(), 0o750); err != nil {
				return err
			}
		}
	}
	for _, o := range orphans {
		if l.driveOf(o.Path) == nil {
			return fmt.Errorf("orphan %s is not on any fixture drive", o.Path)
		}
		name := filepath.Base(o.Path) + ".mkv"
		if err := writeSparse(filepath.Join(o.Path, name), int64(o.Size)); err != nil {
			return err
		}
	}
	return nil
}

func writeSparse(path string, size int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// startMove queues a move the way Radarr/Sonarr's bulk editor does: the
// request returns at once, and a command tracks the copy until it lands.
func (l *library) startMove(app, name string, it *item, root string) {
	l.mu.Lock()
	l.nextCmd++
	cmd := &command{ID: l.nextCmd, Name: name, Status: "started"}
	l.commands[app] = append(l.commands[app], cmd)
	l.mu.Unlock()

	go func() {
		err := l.relocate(it, filepath.Clean(root))
		l.mu.Lock()
		defer l.mu.Unlock()
		if err != nil {
			cmd.Status, cmd.Message = "failed", err.Error()
			logf("%s: moving %q to %s failed: %v", app, it.title, root, err)
			return
		}
		it.root = filepath.Clean(root)
		cmd.Status = "completed"
		logf("%s: moved %q to %s", app, it.title, root)
	}()
}

// runCommand handles every other command (rescans) as finished at once:
// sizes are always read live, so there is nothing to rescan.
func (l *library) runCommand(app, name string) *command {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextCmd++
	cmd := &command{ID: l.nextCmd, Name: name, Status: "completed"}
	l.commands[app] = append(l.commands[app], cmd)
	return cmd
}

const moveTick = 250 * time.Millisecond

// relocate copies an item's folder to root as a cross-device move does:
// the destination files grow over the simulated transfer time, then the
// source is deleted. Radarr/Sonarr keep reporting the old path until then.
func (l *library) relocate(it *item, root string) error {
	l.mu.Lock()
	src, folder, files, size := it.path(), it.folder(), it.files(), it.size
	l.mu.Unlock()

	dst := filepath.Join(root, folder)
	if src == dst {
		return nil
	}
	if l.driveOf(src) == l.driveOf(dst) {
		return os.Rename(src, dst)
	}

	secs := float64(size) / float64(l.sim.MoveThroughput)
	secs = min(max(secs, l.sim.MinMoveSeconds), l.sim.MaxMoveSeconds)
	steps := max(1, int(time.Duration(secs*float64(time.Second))/moveTick))

	if err := os.MkdirAll(dst, 0o750); err != nil {
		return err
	}
	for _, f := range files {
		if err := writeSparse(filepath.Join(dst, f.rel), 0); err != nil {
			return err
		}
	}
	for s := 1; s <= steps; s++ {
		time.Sleep(moveTick)
		for _, f := range files {
			if err := os.Truncate(filepath.Join(dst, f.rel), f.size*int64(s)/int64(steps)); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(src)
}

// historyRecord matches Coldarr's history.json (internal/history.Record).
type historyRecord struct {
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

// writeHistory records every item with moved_days_ago as moved from the
// hot tier onto the tier it sits on now.
func (l *library) writeHistory(path string, tiers []tierSpec) error {
	tierOf := func(root string) (tierSpec, bool) {
		for _, t := range tiers {
			for _, p := range t.Paths {
				if filepath.Clean(p) == root {
					return t, true
				}
			}
		}
		return tierSpec{}, false
	}
	// An item's old hot root is whichever hot path its own app's
	// still-hot items use.
	hotRoot := map[string]string{}
	for _, it := range l.allItems() {
		if t, ok := tierOf(it.root); ok && t.Role == "hot" && hotRoot[it.app] == "" {
			hotRoot[it.app] = it.root
		}
	}

	var records []historyRecord
	for _, it := range l.allItems() {
		if it.movedAt == nil {
			continue
		}
		to, ok := tierOf(it.root)
		if !ok || to.Role != "cold" {
			return fmt.Errorf("%q has moved_days_ago but its root %s is not a cold tier path", it.title, it.root)
		}
		from, ok := tierOf(hotRoot[it.app])
		if !ok {
			return fmt.Errorf("%q has moved_days_ago but no %s item sits on a hot tier to say where it came from", it.title, it.app)
		}
		records = append(records, historyRecord{
			ArrApp:    it.app,
			ItemID:    it.id,
			Title:     it.title,
			FromTier:  from.Name,
			FromPath:  hotRoot[it.app],
			ToTier:    to.Name,
			ToPath:    it.root,
			SizeBytes: it.size,
			MovedAt:   *it.movedAt,
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].MovedAt.Before(records[j].MovedAt) })

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
