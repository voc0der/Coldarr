package torrents

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/qbit"
)

// Removal is downloads done seeding, removed from qBittorrent together:
// every one that's done among a set sharing files.
type Removal struct {
	Name        string
	Names       []string
	Hashes      []string
	ContentPath string
	SizeBytes   int64
	TierName    string
	// DeleteFiles is false when a download that isn't being removed still
	// uses these files: then only the finished downloads leave
	// qBittorrent, and the files stay for KeptFor to go on seeding.
	DeleteFiles bool
	KeptFor     string
	// Seeded is the shortest time any of them has seeded, and Rule the
	// requirement it met.
	Seeded time.Duration
	Rule   string
}

// HeldRemoval is a download done seeding that isn't being removed, and
// why.
type HeldRemoval struct {
	Name   string
	Reason string
}

// RemovalPlan is what removal would do.
type RemovalPlan struct {
	// Removals are this run's, longest-seeded first.
	Removals []Removal
	// Deferred are removals past the per-run cap, left for later runs.
	Deferred []Removal
	Held     []HeldRemoval
}

// PlanRemovals works out which downloads are done seeding and safe to
// remove, from either drive. The guards are the point:
//
//   - only a finished download Coldarr manages, on a downloads tier,
//     whose seeding requirement is known and met - an unscanned tracker,
//     or one that requires seeding forever, never qualifies;
//   - never one an *arr app hasn't confirmed importing, since removing it
//     would lose it - or one still in an *arr queue;
//   - never one any library item (libraryPaths) lives inside, or that
//     lives inside one - insurance for setups where a library and a
//     download share a folder;
//   - never the files of a download that isn't being removed: downloads
//     sharing files are removed together, and if any of them isn't done,
//     the done ones leave qBittorrent but their files stay;
//   - and at most limit downloads a run.
func PlanRemovals(downloads []*Download, libraryPaths []string, limit int) RemovalPlan {
	var plan RemovalPlan
	removable := map[string]bool{}
	for _, d := range downloads {
		if d.TierPath == "" || !d.Seeding.Met {
			continue
		}
		switch d.State {
		case Waiting, Ready, Held, Parked:
		default:
			continue
		}
		if !d.ImportsConfirmed {
			plan.Held = append(plan.Held, HeldRemoval{Name: d.Name, Reason: "done seeding, but " + d.importReason})
			continue
		}
		if lib, ok := overlapping(d.ContentPath, libraryPaths); ok {
			plan.Held = append(plan.Held, HeldRemoval{Name: d.Name, Reason: fmt.Sprintf("done seeding, but it shares a folder with a library item at %s", lib)})
			continue
		}
		removable[d.Hash] = true
	}

	var all []Removal
	for _, cluster := range shareClusters(downloads) {
		var done, others []*Download
		for _, d := range cluster {
			if removable[d.Hash] {
				done = append(done, d)
			} else {
				others = append(others, d)
			}
		}
		if len(done) == 0 {
			continue
		}
		r := Removal{Name: done[0].Name, ContentPath: done[0].ContentPath, TierName: done[0].Tier.Name, DeleteFiles: len(others) == 0, Seeded: done[0].Seeding.Seeded, Rule: done[0].Seeding.Rule}
		for _, d := range done {
			r.Names = append(r.Names, d.Name)
			r.Hashes = append(r.Hashes, d.Hash)
			r.SizeBytes = max(r.SizeBytes, d.Size)
			if d.Seeding.Seeded < r.Seeded {
				r.Seeded, r.Rule = d.Seeding.Seeded, d.Seeding.Rule
			}
		}
		if len(others) > 0 {
			r.KeptFor = others[0].Name
		}
		all = append(all, r)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Seeded != all[j].Seeded {
			return all[i].Seeded > all[j].Seeded
		}
		return all[i].Name < all[j].Name
	})

	count := 0
	for _, r := range all {
		if count+len(r.Hashes) > limit {
			plan.Deferred = append(plan.Deferred, r)
			continue
		}
		count += len(r.Hashes)
		plan.Removals = append(plan.Removals, r)
	}
	return plan
}

// overlapping reports a library path that is path, contains it, or sits
// inside it.
func overlapping(path string, libraryPaths []string) (string, bool) {
	p := cleanPath(path)
	if p == "" {
		return "", false
	}
	for _, lib := range libraryPaths {
		l := cleanPath(lib)
		if l != "" && (isUnder(p, l) || isUnder(l, p)) {
			return lib, true
		}
	}
	return "", false
}

// shareClusters groups downloads whose content overlaps - the same path,
// or one inside another's folder - in content-path order. Downloads with no
// content path share nothing.
func shareClusters(downloads []*Download) [][]*Download {
	byPath := map[string][]*Download{}
	var keys []string
	for _, d := range downloads {
		k := cleanPath(d.ContentPath)
		if k == "" {
			continue
		}
		if _, ok := byPath[k]; !ok {
			keys = append(keys, k)
		}
		byPath[k] = append(byPath[k], d)
	}
	// Sorted with the separator lowest, everything inside a folder comes
	// straight after it - plain string order would put "Show.S01" between
	// "Show" and "Show/ep.mkv", since '.' sorts before '/'. Open folders
	// are kept on a stack, and a path joins the cluster of the outermost
	// one still containing it.
	sort.Slice(keys, func(i, j int) bool { return separatorFirst(keys[i]) < separatorFirst(keys[j]) })
	var clusters [][]*Download
	clusterOf := map[string]int{}
	var stack []string
	for _, k := range keys {
		for len(stack) > 0 && !isUnder(k, stack[len(stack)-1]) {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			clusterOf[k] = len(clusters)
			clusters = append(clusters, nil)
		} else {
			clusterOf[k] = clusterOf[stack[0]]
		}
		clusters[clusterOf[k]] = append(clusters[clusterOf[k]], byPath[k]...)
		stack = append(stack, k)
	}
	return clusters
}

// Remover carries out removals through qBittorrent.
type Remover struct {
	Qbit     *qbit.Client
	Trackers Trackers
	Seeding  config.SeedingConfig
	Log      *history.DownloadLog
	// Now defaults to time.Now.
	Now func() time.Time
}

// Remove removes one removal's downloads - with their files only when
// r.DeleteFiles - after asking qBittorrent again that each is still
// finished, still where the plan found it, and still done seeding: a plan
// can be minutes old, and qBittorrent's seeding time can reset. It then
// checks qBittorrent no longer lists them, and logs the removal.
func (x *Remover) Remove(r Removal) error {
	now := time.Now()
	if x.Now != nil {
		now = x.Now()
	}

	list, err := x.Qbit.TorrentsByHash(r.Hashes)
	if err != nil {
		return fmt.Errorf("checking %s with qBittorrent before removing it: %w", r.Name, err)
	}
	found := indexByHash(list)
	for _, h := range r.Hashes {
		t, ok := found[h]
		switch {
		case !ok:
			return fmt.Errorf("%s is no longer in qBittorrent - nothing was removed", r.Name)
		case !t.Complete() || !qbit.Seeding(t.State):
			return fmt.Errorf("%s isn't finished right now (qBittorrent reports %s) - nothing was removed", t.Name, t.State)
		case cleanPath(t.ContentPath) != cleanPath(r.ContentPath):
			return fmt.Errorf("%s has moved since the plan was made - nothing was removed", t.Name)
		case !seedingStatus(t, x.Trackers, x.Seeding, now).Met:
			return fmt.Errorf("%s isn't done seeding any more - nothing was removed", t.Name)
		}
	}

	if err := x.Qbit.Delete(r.Hashes, r.DeleteFiles); err != nil {
		return fmt.Errorf("removing %s: %w", r.Name, err)
	}
	left, err := x.Qbit.TorrentsByHash(r.Hashes)
	if err != nil {
		return fmt.Errorf("checking %s was removed: %w", r.Name, err)
	}
	if len(left) > 0 {
		return fmt.Errorf("qBittorrent still lists %s after removing it", r.Name)
	}

	ev := history.DownloadEvent{
		Action:   history.DownloadRemoved,
		Names:    r.Names,
		Hashes:   r.Hashes,
		FromPath: r.ContentPath,
		Detail:   fmt.Sprintf("seeded %s (%s)", days(r.Seeded), r.Rule),
		At:       now,
	}
	if r.DeleteFiles {
		ev.SizeBytes = r.SizeBytes
	} else {
		ev.Action = history.DownloadUnlisted
		ev.Detail += fmt.Sprintf("; files kept for %s", r.KeptFor)
	}
	if err := x.Log.Append(ev); err != nil {
		return errors.Join(fmt.Errorf("removed %s, but recording it failed", r.Name), err)
	}
	return nil
}

func days(d time.Duration) string {
	n := d.Hours() / 24
	if n == float64(int(n)) {
		return fmt.Sprintf("%d days", int(n))
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", n), ".0") + " days"
}

// separatorFirst maps the path separator to the lowest byte, so sorting
// by it orders a folder's contents directly after the folder.
func separatorFirst(path string) string {
	return strings.ReplaceAll(path, string(filepath.Separator), "\x00")
}
