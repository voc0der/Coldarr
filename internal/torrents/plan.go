package torrents

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/model"
)

// MoveEntry is one move in a downloads plan: a download, together with
// every cross-seed sharing its files, going from the download drive to
// seed storage in a single qBittorrent request.
type MoveEntry struct {
	// Name is the download's name; Names lists every download moving with
	// it, itself first.
	Name   string
	Names  []string
	Hashes []string
	// SizeBytes is what the move adds to seed storage - the largest
	// member, since cross-seeds share their files.
	SizeBytes int64
	FromTier  string
	// FromPath is the download's save path now, and ContentPath its
	// content; Location and NewContentPath are the same after the move.
	FromPath       string
	ContentPath    string
	Location       string
	NewContentPath string
	ToTier         string
	// ToPath is the seed storage tier path - what's checked for room and
	// watched while the move lands.
	ToPath         string
	MaxUsedPercent float64
	// DisableAutoTMM lists members whose Automatic Torrent Management is
	// switched off before the move (see Download.DisableAutoTMM).
	DisableAutoTMM []string
	// CompletedAt is when the last of the group finished downloading.
	CompletedAt time.Time
}

// MoveTitle, MoveSize, MoveDestination, MovePhase and MoveCeiling describe
// the entry to the shared move engine (internal/mover). Downloads plans
// have one phase: every move is one-way, off the download drive.
func (e MoveEntry) MoveTitle() string       { return e.Name }
func (e MoveEntry) MoveSize() int64         { return e.SizeBytes }
func (e MoveEntry) MoveDestination() string { return e.ToPath }
func (e MoveEntry) MovePhase() int          { return 0 }
func (e MoveEntry) MoveCeiling() float64    { return e.MaxUsedPercent }

// Relabel is a download already on seed storage that still needs the seed
// label (see Download.Relabel).
type Relabel struct {
	Name           string
	Hash           string
	Category       string
	ContentPath    string
	DisableAutoTMM bool
}

// Plan is a downloads plan.
type Plan struct {
	Entries  []MoveEntry
	Relabels []Relabel
	// FinalUsage is each usable path's projected usage once every entry
	// has landed, keyed by cleaned path.
	FinalUsage map[string]diskusage.Usage
	Warnings   []string
}

// PlanFS is what planning needs to know about the disk, as Coldarr sees
// it.
type PlanFS interface {
	FS
	// HasHardLinks reports whether any file under path has another link.
	HasHardLinks(path string) (bool, error)
}

// PlanInput is everything BuildPlan needs.
type PlanInput struct {
	Downloads []*Download
	// Tiers is every configured tier; only downloads tiers are used.
	Tiers []model.Tier
	// Usage holds only usable paths - a missing path is never a
	// destination - with downloads still in progress already counted.
	Usage    map[string]diskusage.Usage
	VolumeOf map[string]uint64
	// Unseen marks (cleaned) paths Coldarr can't see qBittorrent's
	// downloads on; nothing moves into them.
	Unseen map[string]bool
	// Leave names downloads, by hash, to leave where they are because
	// they're about to be removed - a move group stays only if every
	// member is.
	Leave map[string]bool
	FS    PlanFS
}

// BuildPlan works out which ready downloads move where. The longest-
// finished go first - the download drive is a queue, and the oldest have
// waited longest - with the larger first among equals. Each lands on the
// seed storage path with room under its target, the fullest such path
// first so drives fill one at a time; failing that, under its max, which
// is never crossed. A download keeps the folder it sat in below its tier
// path (a category subfolder, say), and never moves onto a file or folder
// already at the same name: qBittorrent would keep that instead of moving
// its own.
func BuildPlan(in PlanInput) *Plan {
	plan := &Plan{}
	// Paths are compared cleaned throughout - a download is located by its
	// cleaned save path, and a configured path may end in a slash.
	working := make(map[string]diskusage.Usage, len(in.Usage))
	for k, v := range in.Usage {
		working[filepath.Clean(k)] = v
	}
	volumeOf := make(map[string]uint64, len(in.VolumeOf))
	for k, v := range in.VolumeOf {
		volumeOf[filepath.Clean(k)] = v
	}
	volumes := diskusage.GroupVolumes(volumeOf)

	type seedPath struct {
		tier model.Tier
		path string
	}
	var seeds []seedPath
	for _, tier := range model.TiersIn(in.Tiers, model.DomainDownloads) {
		if tier.Role != model.RoleCold {
			continue
		}
		for _, p := range tier.Paths {
			p = filepath.Clean(p)
			if _, ok := working[p]; !ok || in.Unseen[p] {
				continue
			}
			seeds = append(seeds, seedPath{tier: tier, path: p})
		}
	}

	left := 0
	for _, group := range readyGroups(in.Downloads) {
		head := group[0]
		if allLeft(group, in.Leave) {
			left++
			continue
		}
		size := int64(0)
		var hashes, names, disable []string
		completed := time.Time{}
		for _, d := range group {
			size = max(size, d.Size)
			hashes = append(hashes, d.Hash)
			names = append(names, d.Name)
			if d.DisableAutoTMM {
				disable = append(disable, d.Hash)
			}
			if d.CompletedOn.After(completed) {
				completed = d.CompletedOn
			}
		}

		relSave, err1 := filepath.Rel(head.TierPath, filepath.Clean(head.SavePath))
		relContent, err2 := filepath.Rel(filepath.Clean(head.SavePath), filepath.Clean(head.ContentPath))
		if err1 != nil || err2 != nil || strings.HasPrefix(relContent, "..") || relContent == "." {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: its content (%s) isn't inside its save path (%s), so Coldarr can't tell where it would land - move it in qBittorrent by hand", head.Name, head.ContentPath, head.SavePath))
			continue
		}

		if linked, err := in.FS.HasHardLinks(head.ContentPath); err == nil && linked {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s has hard links (its files are also somewhere else on the download drive, likely your library): moving it copies it to seed storage but frees nothing on the download drive while the other links remain", head.Name))
		}

		var chosen *seedPath
		var location, newContent string
		collided := ""
		for _, ceiling := range []func(model.Tier) float64{
			func(t model.Tier) float64 { return t.TargetUsedPercent },
			func(t model.Tier) float64 { return t.MaxUsedPercent },
		} {
			candidates := make([]seedPath, 0, len(seeds))
			for _, s := range seeds {
				if fits(working[s.path], size, ceiling(s.tier)) {
					candidates = append(candidates, s)
				}
			}
			sort.SliceStable(candidates, func(i, j int) bool {
				return working[candidates[i].path].UsedPercent > working[candidates[j].path].UsedPercent
			})
			for _, s := range candidates {
				loc := filepath.Join(s.path, relSave)
				content := filepath.Join(loc, relContent)
				if exists, err := in.FS.Exists(content); err != nil || exists {
					collided = content
					continue
				}
				chosen, location, newContent = &s, loc, content
				break
			}
			if chosen != nil {
				break
			}
		}

		if chosen == nil {
			if collided != "" {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("%s: something already exists at %s - qBittorrent would keep it in place of this download's own files, so it isn't moved. If it's left over from an earlier move, the Orphaned Storage page lists it.", head.Name, collided))
			} else {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("no seed storage has room for %s (%.2f GB)", head.Name, gb(size)))
			}
			continue
		}

		plan.Entries = append(plan.Entries, MoveEntry{
			Name:           head.Name,
			Names:          names,
			Hashes:         hashes,
			SizeBytes:      size,
			FromTier:       head.Tier.Name,
			FromPath:       head.SavePath,
			ContentPath:    head.ContentPath,
			Location:       location,
			NewContentPath: newContent,
			ToTier:         chosen.tier.Name,
			ToPath:         chosen.path,
			MaxUsedPercent: chosen.tier.MaxUsedPercent,
			DisableAutoTMM: disable,
			CompletedAt:    completed,
		})
		volumes.Apply(working, head.TierPath, -size)
		volumes.Apply(working, chosen.path, size)
	}

	if left > 0 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("%d download(s) on the download drive are done seeding, so they're left there to be removed rather than moved", left))
	}

	for _, d := range in.Downloads {
		if d.Relabel {
			plan.Relabels = append(plan.Relabels, Relabel{Name: d.Name, Hash: d.Hash, Category: d.Category, ContentPath: d.ContentPath, DisableAutoTMM: d.DisableAutoTMM})
		}
	}

	plan.FinalUsage = working
	return plan
}

// readyGroups returns every group of ready downloads - Classify only ever
// leaves a whole group ready or none of it - longest-finished first, then
// largest, then by name so equal plans come out the same.
func readyGroups(downloads []*Download) [][]*Download {
	byGroup := map[string][]*Download{}
	var keys []string
	for _, d := range downloads {
		if d.State != Ready {
			continue
		}
		if _, ok := byGroup[d.Group]; !ok {
			keys = append(keys, d.Group)
		}
		byGroup[d.Group] = append(byGroup[d.Group], d)
	}
	groups := make([][]*Download, 0, len(keys))
	for _, k := range keys {
		groups = append(groups, byGroup[k])
	}
	finished := func(g []*Download) time.Time {
		var t time.Time
		for _, d := range g {
			if d.CompletedOn.After(t) {
				t = d.CompletedOn
			}
		}
		return t
	}
	size := func(g []*Download) int64 {
		var n int64
		for _, d := range g {
			n = max(n, d.Size)
		}
		return n
	}
	sort.SliceStable(groups, func(i, j int) bool {
		fi, fj := finished(groups[i]), finished(groups[j])
		switch {
		case !fi.Equal(fj):
			return fi.Before(fj)
		case size(groups[i]) != size(groups[j]):
			return size(groups[i]) > size(groups[j])
		}
		return groups[i][0].Name < groups[j][0].Name
	})
	return groups
}

// fits reports whether sizeBytes can land on a path at u without passing
// ceiling percent used, or running out of space to write it.
func fits(u diskusage.Usage, sizeBytes int64, ceiling float64) bool {
	if u.TotalBytes == 0 || sizeBytes < 0 || uint64(sizeBytes) > u.FreeBytes {
		return false
	}
	projected := u.Plus(sizeBytes)
	return projected.UsedPercent <= ceiling && projected.UsedBytes <= u.TotalBytes
}

func gb(b int64) float64 {
	return float64(b) / (1 << 30)
}

func allLeft(group []*Download, leave map[string]bool) bool {
	for _, d := range group {
		if !leave[d.Hash] {
			return false
		}
	}
	return len(leave) > 0
}
