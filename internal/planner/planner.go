// Package planner turns a scored inventory into a concrete move plan: which
// items move from which over-pressure hot path to which cold path, without
// ever pushing a destination past its configured ceiling.
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
	Tiers   []model.Tier
	Usage   map[string]diskusage.Usage
	Items   []ItemEval
	History *history.Store
	Policy  config.PolicyConfig
	Now     time.Time
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

	minMoveBytes := int64(in.Policy.MinMoveSizeGB * (1 << 30))
	cooldown := time.Duration(in.Policy.CooldownDays) * 24 * time.Hour

	plan := &Plan{}

	for _, tier := range in.Tiers {
		if tier.Role != model.RoleHot {
			continue
		}
		for _, path := range tier.Paths {
			usage, ok := working[path]
			if !ok {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("skipping hot path %s: not available (failed existence/mount check)", path))
				continue
			}
			if usage.UsedPercent <= tier.MaxUsedPercent {
				continue
			}

			bytesToFree := int64(float64(usage.TotalBytes) * (usage.UsedPercent - tier.TargetUsedPercent) / 100)
			if bytesToFree <= 0 {
				continue
			}

			candidates := candidatesForPath(in.Items, path, in.History, cooldown, minMoveBytes, in.Now)

			var freed int64
			for _, c := range candidates {
				if freed >= bytesToFree {
					break
				}

				destTier, destPath, ok := pickDestination(in.Tiers, working, c.Item.Type, c.Item.SizeBytes)
				if !ok {
					plan.Warnings = append(plan.Warnings, fmt.Sprintf("no cold destination has room for %q (%s, %.2f GB)", c.Item.Title, c.Item.Type, gb(c.Item.SizeBytes)))
					continue
				}

				plan.Entries = append(plan.Entries, MoveEntry{
					Item:     c.Item,
					FromTier: tier.Name,
					FromPath: path,
					ToTier:   destTier.Name,
					ToPath:   destPath,
					Score:    c.Eval.Score,
					Reasons:  c.Eval.Reasons,
				})

				working[path] = applyDelta(working[path], -c.Item.SizeBytes)
				working[destPath] = applyDelta(working[destPath], c.Item.SizeBytes)
				freed += c.Item.SizeBytes
			}

			if freed < bytesToFree {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf(
					"%s is at %.1f%% (target %.1f%%) but only found %.2f GB of the %.2f GB needed to reach target",
					path, usage.UsedPercent, tier.TargetUsedPercent, gb(freed), gb(bytesToFree)))
			}
		}
	}

	plan.FinalUsage = working
	return plan, nil
}

// candidatesForPath returns cold-eligible items currently stored at path,
// sorted coldest-and-biggest first so a handful of large moves are
// preferred over many small ones.
func candidatesForPath(items []ItemEval, path string, h *history.Store, cooldown time.Duration, minMoveBytes int64, now time.Time) []ItemEval {
	var out []ItemEval
	for _, it := range items {
		if it.Item.RootFolderPath != path {
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

// pickDestination finds the cold-tier path with the most room to spare that
// can still accept sizeBytes without crossing its tier's max usage ceiling.
// Among viable paths it prefers the one that is already fullest, so
// satellites are packed one at a time rather than spread thin.
func pickDestination(tiers []model.Tier, usage map[string]diskusage.Usage, mt model.MediaType, sizeBytes int64) (model.Tier, string, bool) {
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
			if projected.UsedPercent > tier.MaxUsedPercent {
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

func applyDelta(u diskusage.Usage, deltaBytes int64) diskusage.Usage {
	used := int64(u.UsedBytes) + deltaBytes
	if used < 0 {
		used = 0
	}
	u.UsedBytes = uint64(used)
	if u.TotalBytes > u.UsedBytes {
		u.FreeBytes = u.TotalBytes - u.UsedBytes
	} else {
		u.FreeBytes = 0
	}
	if u.TotalBytes > 0 {
		u.UsedPercent = float64(u.UsedBytes) / float64(u.TotalBytes) * 100
	}
	return u
}

func gb(b int64) float64 {
	return float64(b) / (1 << 30)
}
