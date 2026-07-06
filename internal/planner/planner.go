// Package planner turns a scored inventory into a concrete move plan.
// Coldarr does not steer hot storage toward any usage level - hot is
// runoff. The actual goal is to pack cold tiers toward their target usage
// by moving every cold-eligible item currently on hot storage, limited
// only by how much room cold destinations have.
package planner

import (
	"fmt"
	"sort"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/scoring"
)

type ItemEval struct {
	Item model.MediaItem
	Eval scoring.Evaluation
}

// Input is everything the planner needs to build a move plan. Usage must
// only contain paths that have already passed existence/mount-point checks
// - the planner treats a missing entry as "unusable," never as "assume
// empty and write there anyway."
type Input struct {
	Tiers []model.Tier
	Usage map[string]diskusage.Usage
	// VolumeOf maps a path to a device identifier, for paths where it's
	// known. Two paths with the same value are really the same physical
	// volume (however differently named or nested) and share the same
	// capacity - moving something onto one must be reflected on the
	// other too, or the planner would double-count the same disk's free
	// space as if it were two independent pools.
	VolumeOf map[string]uint64
	Items    []ItemEval
	History  *history.Store
	Policy   config.PolicyConfig
	Now      time.Time
}

type MoveEntry struct {
	Item     model.MediaItem
	FromTier string
	FromPath string
	ToTier   string
	ToPath   string
	Score    float64
	Reasons  []string
}

type Plan struct {
	Entries []MoveEntry
	// FinalUsage is the projected per-path usage if every entry in the
	// plan is applied, for reporting before/after space.
	FinalUsage map[string]diskusage.Usage
	Warnings   []string
}

// Build computes a move plan. It never mutates its input; all usage
// bookkeeping happens on a working copy so callers can inspect Input.Usage
// unchanged afterward.
func Build(in Input) (*Plan, error) {
	working := make(map[string]diskusage.Usage, len(in.Usage))
	for k, v := range in.Usage {
		working[k] = v
	}
	volumeGroups := groupByVolume(in.VolumeOf)

	minMoveBytes := int64(in.Policy.MinMoveSizeGB * (1 << 30))
	cooldown := time.Duration(in.Policy.CooldownDays) * 24 * time.Hour

	plan := &Plan{}

	hotPaths := map[string]model.Tier{}
	for _, tier := range in.Tiers {
		if tier.Role != model.RoleHot {
			continue
		}
		for _, path := range tier.Paths {
			hotPaths[path] = tier
			if _, ok := working[path]; !ok {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("skipping hot path %s: not available (failed existence/mount check)", path))
			}
		}
	}

	coldPaths := map[string]model.Tier{}
	for _, tier := range in.Tiers {
		if tier.Role != model.RoleCold {
			continue
		}
		for _, path := range tier.Paths {
			coldPaths[path] = tier
		}
	}

	// Promotions run first: an upcoming item has no business sitting on
	// cold storage - it's about to start actively receiving new episodes/
	// a release, and needs to be back on fast storage before that
	// happens, not packed away on a slow satellite drive. This also
	// frees the cold space it was occupying before the hot->cold pass
	// below considers what else has room to move in.
	for _, c := range upcomingOnCold(in.Items, coldPaths) {
		fromTier := coldPaths[c.Item.RootFolderPath]

		destTier, destPath, ok := pickHotDestination(in.Tiers, working, c.Item.Type, c.Item.SizeBytes)
		if !ok {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("no hot destination has room to move %q back before it's released (%s, %.2f GB)", c.Item.Title, c.Item.Type, gb(c.Item.SizeBytes)))
			continue
		}

		plan.Entries = append(plan.Entries, MoveEntry{
			Item:     c.Item,
			FromTier: fromTier.Name,
			FromPath: c.Item.RootFolderPath,
			ToTier:   destTier.Name,
			ToPath:   destPath,
			Reasons:  []string{"upcoming - misplaced on cold storage, moving back before it's released"},
		})

		applyDeltaToVolume(working, volumeGroups, c.Item.RootFolderPath, -c.Item.SizeBytes)
		applyDeltaToVolume(working, volumeGroups, destPath, c.Item.SizeBytes)
	}

	candidates := coldCandidates(in.Items, hotPaths, in.History, cooldown, minMoveBytes, in.Now)

	for _, c := range candidates {
		fromTier := hotPaths[c.Item.RootFolderPath]

		destTier, destPath, ok := pickDestination(in.Tiers, working, c.Item.Type, c.Item.SizeBytes)
		if !ok {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("no cold destination has room for %q (%s, %.2f GB)", c.Item.Title, c.Item.Type, gb(c.Item.SizeBytes)))
			continue
		}

		plan.Entries = append(plan.Entries, MoveEntry{
			Item:     c.Item,
			FromTier: fromTier.Name,
			FromPath: c.Item.RootFolderPath,
			ToTier:   destTier.Name,
			ToPath:   destPath,
			Score:    c.Eval.Score,
			Reasons:  c.Eval.Reasons,
		})

		applyDeltaToVolume(working, volumeGroups, c.Item.RootFolderPath, -c.Item.SizeBytes)
		applyDeltaToVolume(working, volumeGroups, destPath, c.Item.SizeBytes)
	}

	plan.FinalUsage = working
	return plan, nil
}

// upcomingOnCold returns every item that hasn't been released/premiered
// yet but is currently sitting on a cold-tier path - likely because it was
// imported straight into a root folder Coldarr had previously packed with
// older, already-cold-eligible content, or because a prior season/cut of
// it was moved to cold before this one was announced. Not gated by
// cooldown or minimum move size: unlike the coldness ranking below, this
// is a correctness fix, not a stylistic rebalance, and it's a one-time
// transition per item (once it's released it's no longer "upcoming").
func upcomingOnCold(items []ItemEval, coldPaths map[string]model.Tier) []ItemEval {
	var out []ItemEval
	for _, it := range items {
		if !it.Item.Upcoming {
			continue
		}
		if _, onColdPath := coldPaths[it.Item.RootFolderPath]; !onColdPath {
			continue
		}
		out = append(out, it)
	}
	return out
}

// coldCandidates returns every cold-scored item currently sitting on a hot
// path, sorted coldest-and-biggest first so a handful of large moves are
// preferred over many small ones.
func coldCandidates(items []ItemEval, hotPaths map[string]model.Tier, h *history.Store, cooldown time.Duration, minMoveBytes int64, now time.Time) []ItemEval {
	var out []ItemEval
	for _, it := range items {
		if _, onHotPath := hotPaths[it.Item.RootFolderPath]; !onHotPath {
			continue
		}
		if it.Eval.Decision != scoring.Cold {
			continue
		}
		if it.Item.SizeBytes < minMoveBytes {
			continue
		}
		if h.InCooldown(it.Item.Key(), cooldown, now) {
			continue
		}
		out = append(out, it)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Eval.Score != out[j].Eval.Score {
			return out[i].Eval.Score > out[j].Eval.Score
		}
		return out[i].Item.SizeBytes > out[j].Item.SizeBytes
	})
	return out
}

// pickDestination finds the best cold-tier path to accept sizeBytes. It
// first tries to find room under each cold tier's target (the fill goal
// Coldarr actively packs toward); if nothing has room there, it falls back
// to room under max (the hard ceiling, used as a last resort so an item
// isn't stranded on hot storage just because every tier is already past
// its preferred fill level). Either way, the destination's max is never
// crossed. Among viable paths on a given pass, it prefers the one that is
// already fullest, so satellites are packed one at a time rather than
// spread thin.
func pickDestination(tiers []model.Tier, usage map[string]diskusage.Usage, mt model.MediaType, sizeBytes int64) (model.Tier, string, bool) {
	if tier, path, ok := bestDestination(tiers, usage, mt, sizeBytes, func(t model.Tier) float64 { return t.TargetUsedPercent }); ok {
		return tier, path, true
	}
	return bestDestination(tiers, usage, mt, sizeBytes, func(t model.Tier) float64 { return t.MaxUsedPercent })
}

func bestDestination(tiers []model.Tier, usage map[string]diskusage.Usage, mt model.MediaType, sizeBytes int64, ceiling func(model.Tier) float64) (model.Tier, string, bool) {
	var bestTier model.Tier
	var bestPath string
	var bestUsedPercent float64 = -1
	found := false

	for _, tier := range tiers {
		if tier.Role != model.RoleCold || !tier.AcceptsMediaType(mt) {
			continue
		}
		for _, path := range tier.Paths {
			u, ok := usage[path]
			if !ok {
				continue
			}
			if u.TotalBytes == 0 {
				continue
			}
			projected := applyDelta(u, sizeBytes)
			if projected.UsedPercent > ceiling(tier) {
				continue
			}
			if projected.UsedBytes > u.TotalBytes {
				continue
			}
			if u.UsedPercent > bestUsedPercent {
				bestUsedPercent = u.UsedPercent
				bestTier = tier
				bestPath = path
				found = true
			}
		}
	}

	return bestTier, bestPath, found
}

// pickHotDestination finds a hot-tier path with enough free room to accept
// sizeBytes, for moving an upcoming item back off cold storage (see
// upcomingOnCold). Hot storage is never packed toward a usage level, so
// unlike pickDestination there's no target/max ceiling to respect - just
// enough space to fit. Among viable paths, prefers whichever currently has
// the most free space, spreading rather than concentrating.
func pickHotDestination(tiers []model.Tier, usage map[string]diskusage.Usage, mt model.MediaType, sizeBytes int64) (model.Tier, string, bool) {
	var bestTier model.Tier
	var bestPath string
	var bestFree uint64
	found := false

	for _, tier := range tiers {
		if tier.Role != model.RoleHot || !tier.AcceptsMediaType(mt) {
			continue
		}
		for _, path := range tier.Paths {
			u, ok := usage[path]
			if !ok || u.TotalBytes == 0 {
				continue
			}
			projected := applyDelta(u, sizeBytes)
			if projected.UsedBytes > u.TotalBytes {
				continue
			}
			if !found || u.FreeBytes > bestFree {
				bestFree = u.FreeBytes
				bestTier = tier
				bestPath = path
				found = true
			}
		}
	}

	return bestTier, bestPath, found
}

// groupByVolume builds, for every path with a known device ID, the full
// set of configured paths (including itself) sharing that device.
func groupByVolume(volumeOf map[string]uint64) map[string][]string {
	byDevice := map[uint64][]string{}
	for path, dev := range volumeOf {
		byDevice[dev] = append(byDevice[dev], path)
	}

	groups := make(map[string][]string, len(volumeOf))
	for _, paths := range byDevice {
		for _, p := range paths {
			groups[p] = paths
		}
	}
	return groups
}

// applyDeltaToVolume applies deltaBytes to path's usage and to every other
// path sharing its physical volume, since they're really one capacity
// pool - a move affecting one must be reflected on all of them.
func applyDeltaToVolume(working map[string]diskusage.Usage, groups map[string][]string, path string, deltaBytes int64) {
	siblings, ok := groups[path]
	if !ok {
		siblings = []string{path}
	}
	for _, p := range siblings {
		if u, ok := working[p]; ok {
			working[p] = applyDelta(u, deltaBytes)
		}
	}
}

func applyDelta(u diskusage.Usage, deltaBytes int64) diskusage.Usage {
	used := int64(u.UsedBytes) + deltaBytes
	if used < 0 {
		used = 0
	}
	free := int64(u.FreeBytes) - deltaBytes
	if free < 0 {
		free = 0
	}
	u.UsedBytes = uint64(used)
	u.FreeBytes = uint64(free)
	u.UsedPercent = diskusage.PercentUsed(u.UsedBytes, u.FreeBytes)
	return u
}

func gb(b int64) float64 {
	return float64(b) / (1 << 30)
}
