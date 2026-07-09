// Package planner turns a scored inventory into a concrete move plan.
// Coldarr does not proactively steer hot storage toward any usage level -
// hot is runoff. The actual goal is to pack cold tiers toward their target
// usage by moving every cold-eligible item currently on hot storage,
// limited only by how much room cold destinations have. Hot destinations
// still respect a ceiling when accepting a reclaim (see
// defaultHotMaxUsedPercent) - "runoff" means not actively packed toward a
// level, not "safe to fill completely."
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
	// FromRole is the role FromTier had - descriptive metadata for
	// whether this is a reclaim (cold) or an ordinary fill (hot).
	// Execution order is driven by Phase below, not this field.
	FromRole model.TierRole
	FromPath string
	ToTier   string
	ToPath   string
	// Phase is a strictly increasing sequence number Build assigns as it
	// resolves mutual space dependencies between reclaims and fills (see
	// the comment above the round loop in Build): an entry with a lower
	// Phase may free space (or otherwise change conditions) that a
	// higher-Phase entry's placement depended on. The mover (see
	// internal/mover) executes phases in ascending order, waiting for
	// each one to fully land on disk before starting the next - it
	// cannot just group by direction, since a later round's reclaim can
	// depend on an earlier round's fill having actually happened, not
	// merely having been planned.
	Phase   int
	Score   float64
	Reasons []string
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

	// Reclaims (cold->hot, for items misplaced on cold - see
	// misplacedOnCold) and fills (hot->cold, ordinary cold-eligible items
	// - see coldCandidates) can each depend on space the other frees: a
	// fill needs cold room a reclaim frees, and a reclaim needs hot room
	// a fill frees. Neither candidate list is sorted by who depends on
	// whom, so both passes run repeatedly in rounds against the same
	// working usage, retrying whatever didn't fit last time, until a
	// full round places nothing new. Since every still-pending candidate
	// gets a genuine retry every round, nothing lands in a later round
	// unless it truly couldn't have fit any earlier one - so the Phase
	// each entry is stamped with (see MoveEntry.Phase) never
	// over-serializes the mover's actual execution beyond what the real
	// dependency requires.
	pendingReclaims := misplacedOnCold(in.Items, coldPaths)
	pendingFills := coldCandidates(in.Items, hotPaths, in.History, cooldown, minMoveBytes, in.Now)

	// maxRounds is a defensive bound, not an expected path: each round
	// that makes progress places at least one more candidate, so the
	// loop naturally terminates (via the break below) well before this
	// many iterations. It exists only to guarantee termination even if a
	// future change broke that invariant.
	maxRounds := len(pendingReclaims) + len(pendingFills) + 1
	phase := 0
	for round := 0; round < maxRounds; round++ {
		var reclaimEntries, fillEntries []MoveEntry
		var reclaimsProgressed, fillsProgressed bool

		reclaimEntries, pendingReclaims, reclaimsProgressed = attemptReclaims(pendingReclaims, in.Tiers, working, volumeGroups, phase)
		plan.Entries = append(plan.Entries, reclaimEntries...)
		phase++

		fillEntries, pendingFills, fillsProgressed = attemptFills(pendingFills, hotPaths, in.Tiers, working, volumeGroups, phase)
		plan.Entries = append(plan.Entries, fillEntries...)
		phase++

		if !reclaimsProgressed && !fillsProgressed {
			break
		}
	}

	// Whatever is still pending after a full round made no progress
	// genuinely doesn't fit no matter the ordering - warn once, for the
	// final leftovers only (not per-round, which would misreport
	// candidates that go on to succeed in a later round).
	for _, r := range pendingReclaims {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("no hot destination has room to move %q back (%s, %.2f GB): %s", r.Item.Item.Title, r.Item.Item.Type, gb(r.Item.Item.SizeBytes), r.Reason))
	}
	for _, c := range pendingFills {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("no cold destination has room for %q (%s, %.2f GB)", c.Item.Title, c.Item.Type, gb(c.Item.SizeBytes)))
	}

	plan.FinalUsage = working
	return plan, nil
}

// attemptReclaims tries to place every still-pending reclaim candidate
// against the current working usage, stamping each successfully-placed
// entry with phase (see MoveEntry.Phase). Returns the entries placed
// this call, whichever candidates still have nowhere to go, and whether
// any progress was made this call.
func attemptReclaims(pending []misplacedOnColdItem, tiers []model.Tier, working map[string]diskusage.Usage, volumeGroups map[string][]string, phase int) (entries []MoveEntry, stillPending []misplacedOnColdItem, progressed bool) {
	for _, r := range pending {
		destTier, destPath, ok := pickHotDestination(tiers, working, r.Item.Item.Type, r.Item.Item.SizeBytes)
		if !ok {
			stillPending = append(stillPending, r)
			continue
		}

		entries = append(entries, MoveEntry{
			Item:     r.Item.Item,
			FromTier: r.FromTier.Name,
			FromRole: r.FromTier.Role,
			FromPath: r.Item.Item.RootFolderPath,
			ToTier:   destTier.Name,
			ToPath:   destPath,
			Phase:    phase,
			Reasons:  []string{r.Reason},
		})
		progressed = true

		applyDeltaToVolume(working, volumeGroups, r.Item.Item.RootFolderPath, -r.Item.Item.SizeBytes)
		applyDeltaToVolume(working, volumeGroups, destPath, r.Item.Item.SizeBytes)
	}
	return entries, stillPending, progressed
}

// attemptFills tries to place every still-pending fill candidate against
// the current working usage, stamping each successfully-placed entry
// with phase (see MoveEntry.Phase). Returns the entries placed this
// call, whichever candidates still have nowhere to go, and whether any
// progress was made this call.
func attemptFills(pending []ItemEval, hotPaths map[string]model.Tier, tiers []model.Tier, working map[string]diskusage.Usage, volumeGroups map[string][]string, phase int) (entries []MoveEntry, stillPending []ItemEval, progressed bool) {
	for _, c := range pending {
		fromTier := hotPaths[c.Item.RootFolderPath]

		destTier, destPath, ok := pickDestination(tiers, working, c.Item.Type, c.Item.SizeBytes)
		if !ok {
			stillPending = append(stillPending, c)
			continue
		}

		entries = append(entries, MoveEntry{
			Item:     c.Item,
			FromTier: fromTier.Name,
			FromRole: fromTier.Role,
			FromPath: c.Item.RootFolderPath,
			ToTier:   destTier.Name,
			ToPath:   destPath,
			Phase:    phase,
			Score:    c.Eval.Score,
			Reasons:  c.Eval.Reasons,
		})
		progressed = true

		applyDeltaToVolume(working, volumeGroups, c.Item.RootFolderPath, -c.Item.SizeBytes)
		applyDeltaToVolume(working, volumeGroups, destPath, c.Item.SizeBytes)
	}
	return entries, stillPending, progressed
}

// misplacedOnColdItem pairs an item that needs to be pulled back from cold
// storage with the specific reason it doesn't belong there and the cold
// tier it currently occupies.
type misplacedOnColdItem struct {
	Item     ItemEval
	Reason   string
	FromTier model.Tier
}

// misplacedOnCold returns every item currently sitting on a cold-tier path
// whose contents aren't actually settled yet, so cold - which Coldarr packs
// tight toward its target/max on purpose - is the worst possible place for
// it:
//
//   - Upcoming: hasn't been released/premiered yet, and is about to start
//     actively receiving new episodes/a release. Likely on cold because it
//     was imported straight into a root folder Coldarr had previously
//     packed with older, already-cold-eligible content, or because a prior
//     season/cut of it was moved to cold before this one was announced.
//
//   - Monitored with an unmet quality cutoff: the owning Arr app will keep
//     searching and eventually replace the file with a bigger upgrade,
//     wherever it happens to live. It may have reached cold before this
//     check existed, or become grow-risk afterward (a series un-ends, a
//     quality profile's cutoff changes) - either way, leaving it on an
//     already near-full cold tier risks that tier overflowing once the
//     upgrade lands.
//
// Not gated by cooldown or minimum move size: unlike the coldness ranking
// below, this is a correctness fix, not a stylistic rebalance, and it's a
// one-time transition per item (once it's released, or upgraded to meet
// its cutoff, it no longer matches either condition).
func misplacedOnCold(items []ItemEval, coldPaths map[string]model.Tier) []misplacedOnColdItem {
	var out []misplacedOnColdItem
	for _, it := range items {
		fromTier, onColdPath := coldPaths[it.Item.RootFolderPath]
		if !onColdPath {
			continue
		}
		switch {
		case it.Item.Upcoming:
			out = append(out, misplacedOnColdItem{Item: it, Reason: "upcoming - misplaced on cold storage, moving back before it's released", FromTier: fromTier})
		case it.Item.Monitored && it.Item.QualityCutoffNotMet:
			out = append(out, misplacedOnColdItem{Item: it, Reason: "quality cutoff not met - misplaced on cold storage, moving back so the eventual upgrade lands on fast storage", FromTier: fromTier})
		}
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

// defaultHotMaxUsedPercent is the ceiling pickHotDestination enforces on a
// hot tier that hasn't set its own MaxUsedPercent (the documented default
// of zero). Reclaim exists to give a grow-risk item room to actually grow
// - packing its destination to literal 100% used would defeat that (no
// headroom left for the item to grow into, or for the filesystem's own
// overhead and in-flight writes) and risks real filesystem trouble
// besides. 97% leaves meaningful headroom while still letting reclaim
// work on the tight, mostly-full setups Coldarr targets.
const defaultHotMaxUsedPercent = 97.0

// pickHotDestination finds a hot-tier path with enough free room to accept
// sizeBytes, for moving an upcoming item back off cold storage (see
// upcomingOnCold). Hot storage is never proactively packed toward a usage
// level the way cold tiers are packed toward TargetUsedPercent - but a
// destination's projected usage still may not cross its ceiling: the
// tier's own MaxUsedPercent if it has set one, else
// defaultHotMaxUsedPercent. Among viable paths, prefers whichever
// currently has the most free space, spreading rather than concentrating.
func pickHotDestination(tiers []model.Tier, usage map[string]diskusage.Usage, mt model.MediaType, sizeBytes int64) (model.Tier, string, bool) {
	var bestTier model.Tier
	var bestPath string
	var bestFree uint64
	found := false

	for _, tier := range tiers {
		if tier.Role != model.RoleHot || !tier.AcceptsMediaType(mt) {
			continue
		}
		ceiling := tier.MaxUsedPercent
		if ceiling <= 0 {
			ceiling = defaultHotMaxUsedPercent
		}
		for _, path := range tier.Paths {
			u, ok := usage[path]
			if !ok || u.TotalBytes == 0 {
				continue
			}
			projected := applyDelta(u, sizeBytes)
			if projected.UsedPercent > ceiling {
				continue
			}
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
	used := int64(u.UsedBytes) + deltaBytes //nolint:gosec // disk usage byte counts never approach int64 overflow range
	if used < 0 {
		used = 0
	}
	free := int64(u.FreeBytes) - deltaBytes //nolint:gosec // disk usage byte counts never approach int64 overflow range
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
