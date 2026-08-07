// Package planner turns a scored inventory into a concrete move plan.
// Coldarr does not proactively steer hot storage toward any usage level -
// hot is runoff. The actual goal is to pack cold tiers toward their target
// usage by moving every cold-eligible item currently on hot storage,
// limited only by how much room cold destinations have. Hot destinations
// still respect a ceiling when accepting a reclaim (see
// model.DefaultHotMaxUsedPercent) - "runoff" means not actively packed
// toward a level, not "safe to fill completely."
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
	// Phase is a nondecreasing batch number Build assigns as it resolves
	// mutual space dependencies between reclaims and fills (see the
	// comment above the round loop in Build): entries in the same phase
	// are independent and may run concurrently; a lower phase may free
	// space a higher-phase entry's placement depended on. The mover (see
	// internal/mover) executes phases in ascending order, waiting for
	// each one to fully land on disk before starting the next - it
	// cannot just group by direction, since a later round's reclaim can
	// depend on an earlier round's fill having actually happened, not
	// merely having been planned.
	Phase int
	// MaxUsedPercent is the destination's effective hard ceiling at plan
	// time. Keeping it on the entry lets the mover reject a destination
	// whose real usage drifted past the same limit before execution.
	MaxUsedPercent float64
	Score          float64
	Reasons        []string
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
			if _, ok := working[path]; !ok {
				plan.Warnings = append(plan.Warnings, fmt.Sprintf("skipping hot path %s: not available (failed existence/mount check)", path))
				continue
			}
			hotPaths[path] = tier
		}
	}

	coldPaths := map[string]model.Tier{}
	for _, tier := range in.Tiers {
		if tier.Role != model.RoleCold {
			continue
		}
		for _, path := range tier.Paths {
			if _, ok := working[path]; ok {
				coldPaths[path] = tier
			}
		}
	}

	// Reclaims (cold->hot, for items misplaced on cold - see
	// misplacedOnCold) and fills (hot->cold, ordinary cold-eligible items
	// - see coldCandidates) can each depend on space the other frees. A
	// phase may reserve room for several independent moves, but a move is
	// never allowed to spend space another move in that same phase only
	// promises to free: the mover executes a phase concurrently. Fills
	// stop as soon as their completed phase can accommodate the current
	// reclaim-wave goal, so the first newly safe hot-bound item runs
	// immediately and later work is batched without being bunched behind
	// every unrelated fill.
	pendingReclaims := misplacedOnCold(in.Items, coldPaths)
	pendingFills := coldCandidates(in.Items, hotPaths, in.History, cooldown, minMoveBytes, in.Now)

	// maxRounds is a defensive bound, not an expected path: each round
	// that makes progress places at least one more candidate, so the
	// loop naturally terminates (via the break below) well before this
	// many iterations. It exists only to guarantee termination even if a
	// future change broke that invariant.
	maxRounds := len(pendingReclaims) + len(pendingFills) + 1
	phase := 0
	// The first reclaim wave runs as soon as one blocked item becomes
	// placeable. Later wave goals grow by four, keeping hot-bound work
	// responsive while limiting a large library to logarithmically many
	// settle-and-wait boundaries rather than one per reclaim.
	reclaimBatchGoal := 1
	for round := 0; round < maxRounds; round++ {
		var reclaimEntries, fillEntries []MoveEntry
		var reclaimsProgressed, fillsProgressed bool

		reclaimEntries, pendingReclaims, reclaimsProgressed = attemptReclaims(pendingReclaims, in.Tiers, working, volumeGroups, phase)
		plan.Entries = append(plan.Entries, reclaimEntries...)
		if reclaimsProgressed {
			phase++
			if reclaimBatchGoal < len(pendingReclaims) {
				if reclaimBatchGoal > len(pendingReclaims)/4 {
					reclaimBatchGoal = len(pendingReclaims)
				} else {
					reclaimBatchGoal *= 4
				}
			}
		}

		fillEntries, pendingFills, fillsProgressed = attemptFills(pendingFills, pendingReclaims, reclaimBatchGoal, hotPaths, in.Tiers, working, volumeGroups, phase)
		plan.Entries = append(plan.Entries, fillEntries...)
		if fillsProgressed {
			phase++
		}

		if !reclaimsProgressed && !fillsProgressed {
			break
		}
	}

	// Whatever is still pending after a full round made no progress has no
	// destination in the final projected state. Warn once for those final
	// leftovers only (not per-round, which would misreport candidates that
	// go on to succeed in a later round).
	for _, r := range pendingReclaims {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("no hot destination has room to move %q back (%s, %.2f GB): %s", r.Item.Item.Title, r.Item.Item.Type, gb(r.Item.Item.SizeBytes), r.Reason))
	}
	for _, c := range pendingFills {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("no cold destination has room for %q (%s, %.2f GB)", c.Item.Title, c.Item.Type, gb(c.Item.SizeBytes)))
	}

	plan.FinalUsage = working
	return plan, nil
}

// attemptReclaims places the largest feasible reclaim set in one phase (up to
// the bounded search performed by allocateReclaims). Destination reservations
// made in this call count immediately; source space is credited only to
// working, for subsequent phases, because same-phase moves execute
// concurrently.
func attemptReclaims(pending []misplacedOnColdItem, tiers []model.Tier, working map[string]diskusage.Usage, volumeGroups map[string][]string, phase int) (entries []MoveEntry, stillPending []misplacedOnColdItem, progressed bool) {
	placements, stillPending := allocateReclaims(pending, tiers, working, volumeGroups)
	for _, placement := range placements {
		r := pending[placement.candidateIndex]
		entries = append(entries, MoveEntry{
			Item:           r.Item.Item,
			FromTier:       r.FromTier.Name,
			FromRole:       r.FromTier.Role,
			FromPath:       r.Item.Item.RootFolderPath,
			ToTier:         placement.destTier.Name,
			ToPath:         placement.destPath,
			Phase:          phase,
			MaxUsedPercent: placement.destTier.EffectiveMaxUsedPercent(),
			Reasons:        []string{r.Reason},
		})

		applyDeltaToVolume(working, volumeGroups, r.Item.Item.RootFolderPath, -r.Item.Item.SizeBytes)
		applyDeltaToVolume(working, volumeGroups, placement.destPath, r.Item.Item.SizeBytes)
	}
	return entries, stillPending, len(entries) > 0
}

const reclaimAllocationSearchBudget = 50_000

type reclaimPlacement struct {
	candidateIndex int
	destTier       model.Tier
	destPath       string
}

// allocateReclaims seeds a solution with the fast constrained/largest-first
// heuristic, then uses bounded backtracking to repair bin-packing mistakes.
// A full feasible allocation terminates immediately; genuinely difficult
// large batches cannot exceed reclaimAllocationSearchBudget search nodes.
func allocateReclaims(pending []misplacedOnColdItem, tiers []model.Tier, usage map[string]diskusage.Usage, volumeGroups map[string][]string) ([]reclaimPlacement, []misplacedOnColdItem) {
	best := greedyReclaimPlacements(pending, tiers, usage, volumeGroups)
	if len(best) < len(pending) {
		order := make([]int, 0, len(pending))
		optionCounts := make([]int, len(pending))
		for i, candidate := range pending {
			optionCounts[i] = len(hotDestinationOptions(tiers, usage, candidate.Item.Item.Type, candidate.Item.Item.SizeBytes))
			// Capacity only shrinks during a reclaim phase, so a candidate
			// with no option now can never become placeable in this search.
			if optionCounts[i] > 0 {
				order = append(order, i)
			}
		}
		sort.SliceStable(order, func(i, j int) bool {
			left, right := optionCounts[order[i]], optionCounts[order[j]]
			switch {
			case left != right:
				return left < right
			default:
				return pending[order[i]].Item.Item.SizeBytes > pending[order[j]].Item.Item.SizeBytes
			}
		})

		capacity := copyUsage(usage)
		current := make([]reclaimPlacement, 0, len(pending))
		nodes := 0
		var search func(int)
		search = func(position int) {
			if len(best) == len(order) || nodes >= reclaimAllocationSearchBudget {
				return
			}
			nodes++
			if len(current)+len(order)-position <= len(best) {
				return
			}
			if position == len(order) {
				best = append([]reclaimPlacement(nil), current...)
				return
			}

			candidateIndex := order[position]
			candidate := pending[candidateIndex]
			options := hotDestinationOptions(tiers, capacity, candidate.Item.Item.Type, candidate.Item.Item.SizeBytes)
			sort.SliceStable(options, func(i, j int) bool {
				return options[i].remainingRoom < options[j].remainingRoom
			})
			for _, option := range options {
				current = append(current, reclaimPlacement{candidateIndex: candidateIndex, destTier: option.tier, destPath: option.path})
				applyDeltaToVolume(capacity, volumeGroups, option.path, candidate.Item.Item.SizeBytes)
				search(position + 1)
				applyDeltaToVolume(capacity, volumeGroups, option.path, -candidate.Item.Item.SizeBytes)
				current = current[:len(current)-1]
				if len(best) == len(order) || nodes >= reclaimAllocationSearchBudget {
					return
				}
			}
			search(position + 1)
		}
		search(0)
	}

	placed := make([]bool, len(pending))
	for _, placement := range best {
		placed[placement.candidateIndex] = true
	}
	stillPending := make([]misplacedOnColdItem, 0, len(pending)-len(best))
	for i, candidate := range pending {
		if !placed[i] {
			stillPending = append(stillPending, candidate)
		}
	}
	return best, stillPending
}

func greedyReclaimPlacements(pending []misplacedOnColdItem, tiers []model.Tier, usage map[string]diskusage.Usage, volumeGroups map[string][]string) []reclaimPlacement {
	capacity := copyUsage(usage)
	remaining := append([]misplacedOnColdItem(nil), pending...)
	indices := make([]int, len(pending))
	for i := range indices {
		indices[i] = i
	}

	var placements []reclaimPlacement
	for len(remaining) > 0 {
		i, destTier, destPath, ok := nextReclaim(remaining, tiers, capacity)
		if !ok {
			break
		}
		placements = append(placements, reclaimPlacement{candidateIndex: indices[i], destTier: destTier, destPath: destPath})
		applyDeltaToVolume(capacity, volumeGroups, destPath, remaining[i].Item.Item.SizeBytes)
		remaining = append(remaining[:i], remaining[i+1:]...)
		indices = append(indices[:i], indices[i+1:]...)
	}
	return placements
}

// attemptFills places fills until they are exhausted or until the completed
// phase has enough room for the reclaim batch this fill phase can enable. As
// with reclaims, same-phase destination reservations count immediately while
// source frees become usable only by later phases.
func attemptFills(pending []ItemEval, pendingReclaims []misplacedOnColdItem, reclaimBatchGoal int, hotPaths map[string]model.Tier, tiers []model.Tier, working map[string]diskusage.Usage, volumeGroups map[string][]string, phase int) (entries []MoveEntry, stillPending []ItemEval, progressed bool) {
	reclaimTarget := reclaimBatchTarget(pending, pendingReclaims, reclaimBatchGoal, tiers, working, volumeGroups)
	capacity := copyUsage(working)
	for i, c := range pending {
		fromTier := hotPaths[c.Item.RootFolderPath]

		destTier, destPath, ok := pickDestination(tiers, capacity, c.Item.Type, c.Item.SizeBytes)
		if !ok {
			stillPending = append(stillPending, c)
			continue
		}

		entries = append(entries, MoveEntry{
			Item:           c.Item,
			FromTier:       fromTier.Name,
			FromRole:       fromTier.Role,
			FromPath:       c.Item.RootFolderPath,
			ToTier:         destTier.Name,
			ToPath:         destPath,
			Phase:          phase,
			MaxUsedPercent: destTier.EffectiveMaxUsedPercent(),
			Score:          c.Eval.Score,
			Reasons:        c.Eval.Reasons,
		})
		progressed = true

		applyDeltaToVolume(capacity, volumeGroups, destPath, c.Item.SizeBytes)
		applyDeltaToVolume(working, volumeGroups, c.Item.RootFolderPath, -c.Item.SizeBytes)
		applyDeltaToVolume(working, volumeGroups, destPath, c.Item.SizeBytes)

		if reclaimTarget > 0 && placeableReclaimCount(pendingReclaims, tiers, working, volumeGroups) >= reclaimTarget {
			stillPending = append(stillPending, pending[i+1:]...)
			break
		}
	}
	return entries, stillPending, progressed
}

// reclaimBatchTarget calculates how many reclaims the subsequent phase could
// place if every independently safe fill ran first, capped at the current
// geometric wave goal. attemptFills stops once it reaches that target, leaving
// additional fills for later instead of delaying hot-bound work for no gain.
func reclaimBatchTarget(pendingFills []ItemEval, pendingReclaims []misplacedOnColdItem, reclaimBatchGoal int, tiers []model.Tier, usage map[string]diskusage.Usage, volumeGroups map[string][]string) int {
	if len(pendingReclaims) == 0 {
		return 0
	}
	capacity := copyUsage(usage)
	projected := copyUsage(usage)
	for _, fill := range pendingFills {
		_, destPath, ok := pickDestination(tiers, capacity, fill.Item.Type, fill.Item.SizeBytes)
		if !ok {
			continue
		}
		applyDeltaToVolume(capacity, volumeGroups, destPath, fill.Item.SizeBytes)
		applyDeltaToVolume(projected, volumeGroups, fill.Item.RootFolderPath, -fill.Item.SizeBytes)
		applyDeltaToVolume(projected, volumeGroups, destPath, fill.Item.SizeBytes)
	}
	possible := placeableReclaimCount(pendingReclaims, tiers, projected, volumeGroups)
	if possible > reclaimBatchGoal {
		return reclaimBatchGoal
	}
	return possible
}

func placeableReclaimCount(pending []misplacedOnColdItem, tiers []model.Tier, usage map[string]diskusage.Usage, volumeGroups map[string][]string) int {
	capacity := copyUsage(usage)
	remaining := append([]misplacedOnColdItem(nil), pending...)
	placed := 0
	for len(remaining) > 0 {
		i, _, path, ok := nextReclaim(remaining, tiers, capacity)
		if !ok {
			break
		}
		sizeBytes := remaining[i].Item.Item.SizeBytes
		applyDeltaToVolume(capacity, volumeGroups, path, sizeBytes)
		remaining = append(remaining[:i], remaining[i+1:]...)
		placed++
	}
	return placed
}

func nextReclaim(pending []misplacedOnColdItem, tiers []model.Tier, usage map[string]diskusage.Usage) (int, model.Tier, string, bool) {
	bestIndex := -1
	bestOptions := 0
	var bestTier model.Tier
	var bestPath string

	for i, candidate := range pending {
		options := hotDestinationOptions(tiers, usage, candidate.Item.Item.Type, candidate.Item.Item.SizeBytes)
		if len(options) == 0 {
			continue
		}
		if bestIndex >= 0 && (len(options) > bestOptions ||
			(len(options) == bestOptions && candidate.Item.Item.SizeBytes <= pending[bestIndex].Item.Item.SizeBytes)) {
			continue
		}
		bestIndex = i
		bestOptions = len(options)
		bestTier, bestPath = bestHotDestination(options)
	}

	return bestIndex, bestTier, bestPath, bestIndex >= 0
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
// that doesn't belong there - either because its contents aren't actually
// settled yet, so cold (which Coldarr packs tight toward its target/max on
// purpose) is the worst possible place for it, or because a user has said
// outright that they want it on hot:
//
//   - Marked Favorite in Jellyfin: someone in the household asked for this
//     title to live on fast storage. Typically it reached cold before it
//     was favorited - Coldarr demoted it, the user noticed, and starred it
//     precisely to undo that - so the next plan has to be willing to bring
//     it back rather than only to leave future favorites alone.
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
// below, none of these are a stylistic rebalance. The grow-risk states are
// one-time transitions (once released, or upgraded to meet its cutoff, an
// item stops matching), and a Favorite is a direct user instruction that
// should take effect on the very next plan - most of all when Coldarr moved
// the item minutes ago, which is exactly what prompted the favoriting.
// Protected remains absolute, however: active imports, never-move tags, and
// other protected states must not be reclaimed even for a favorite.
func misplacedOnCold(items []ItemEval, coldPaths map[string]model.Tier) []misplacedOnColdItem {
	var out []misplacedOnColdItem
	for _, it := range items {
		fromTier, onColdPath := coldPaths[it.Item.RootFolderPath]
		if !onColdPath || it.Eval.Decision == scoring.Protected {
			continue
		}
		switch {
		case it.Item.JellyfinFavorite:
			out = append(out, misplacedOnColdItem{Item: it, Reason: "marked Favorite in Jellyfin - moving back to hot storage", FromTier: fromTier})
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
			if !hasWritableSpace(u, sizeBytes) {
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

type hotDestinationOption struct {
	tier          model.Tier
	path          string
	remainingRoom float64
}

// hotDestinationOptions returns every hot-tier path with enough free room
// to accept sizeBytes, for moving a grow-risk item back off cold storage
// (see misplacedOnCold). Hot storage is never proactively packed toward a
// usage level the way cold tiers are packed toward TargetUsedPercent - but
// a destination's projected usage still may not cross its ceiling: the
// tier's own MaxUsedPercent if it has set one, else the model default.
// Callers that need a single destination pick with bestHotDestination.
func hotDestinationOptions(tiers []model.Tier, usage map[string]diskusage.Usage, mt model.MediaType, sizeBytes int64) []hotDestinationOption {
	var options []hotDestinationOption

	for _, tier := range tiers {
		if tier.Role != model.RoleHot || !tier.AcceptsMediaType(mt) {
			continue
		}
		ceiling := tier.EffectiveMaxUsedPercent()
		for _, path := range tier.Paths {
			u, ok := usage[path]
			if !ok || u.TotalBytes == 0 {
				continue
			}
			if !hasWritableSpace(u, sizeBytes) {
				continue
			}
			projected := applyDelta(u, sizeBytes)
			if projected.UsedPercent > ceiling {
				continue
			}
			if projected.UsedBytes > u.TotalBytes {
				continue
			}
			writableRoom := float64(u.FreeBytes)
			ceilingRoom := float64(u.UsedBytes+u.FreeBytes)*ceiling/100 - float64(u.UsedBytes)
			if ceilingRoom < writableRoom {
				writableRoom = ceilingRoom
			}
			options = append(options, hotDestinationOption{
				tier:          tier,
				path:          path,
				remainingRoom: writableRoom - float64(sizeBytes),
			})
		}
	}

	return options
}

// bestHotDestination chooses the tightest fit among options, preserving
// larger slots for reclaims that cannot fit anywhere else.
func bestHotDestination(options []hotDestinationOption) (model.Tier, string) {
	best := options[0]
	for _, option := range options[1:] {
		if option.remainingRoom < best.remainingRoom {
			best = option
		}
	}
	return best.tier, best.path
}

func hasWritableSpace(u diskusage.Usage, sizeBytes int64) bool {
	return sizeBytes >= 0 && uint64(sizeBytes) <= u.FreeBytes
}

func copyUsage(usage map[string]diskusage.Usage) map[string]diskusage.Usage {
	cloned := make(map[string]diskusage.Usage, len(usage))
	for path, u := range usage {
		cloned[path] = u
	}
	return cloned
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
