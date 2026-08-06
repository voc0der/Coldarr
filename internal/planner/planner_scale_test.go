package planner

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/scoring"
)

// This file holds the deliberately large scenarios. Every other test in
// this package isolates one rule with two or three items; these run whole
// fleets and thousands of items, because the behaviours that only appear
// at real-deployment scale - free space double-counted across two root
// folders on the same disk, a plan degenerating into thousands of
// sequential mover phases, a destination creeping past its ceiling after
// a few hundred accumulated placements, candidates quietly vanishing
// between rounds - are exactly the behaviours a three-item test cannot
// show.
//
// Two fleets are modelled, because their bottlenecks are opposite ends of
// the same problem:
//
//   - scaleDrives: 120 TB, one 20 TB hot drive against 40/32/18/10 TB of
//     cold. Cold has room to spare, so nearly everything eligible moves.
//   - bigHotScaleDrives: 149 TB, a 105 TB hot array against five small
//     satellites (4x10 TB + 4 TB), most of them already past their max.
//     Cold can absorb only a few percent of what hot wants to shed.

// scaleDrive is one physical disk in a large-library scenario. Each disk
// carries two configured root folders, one for TV and one for movies,
// that really share a single pool of capacity - the layout a real
// deployment has, and the one that catches the same free bytes being
// counted once per root folder.
type scaleDrive struct {
	name      string
	dev       uint64
	role      model.TierRole
	total     uint64
	usedPct   float64
	tvPath    string
	moviePath string
}

// scaleDrives is the 120 TB fleet: 20 TB hot, plus 40/32/18/10 TB cold.
// The sizes are deliberately unequal so destination selection has to
// actually choose between drives rather than getting the same answer
// whatever it does.
func scaleDrives() []scaleDrive {
	return []scaleDrive{
		// Hot starts at 98.6% used: past the hot tier's default
		// ceiling, though with ~280 GB free it has far more raw bytes
		// than any single reclaim needs. Nothing can be reclaimed onto
		// it until fills bring it back under the ceiling.
		{name: "hot", dev: 100, role: model.RoleHot, total: 20 * tib, usedPct: 98.6, tvPath: "/data/tv", moviePath: "/data/movies"},
		{name: "cold-a", dev: 101, role: model.RoleCold, total: 40 * tib, usedPct: 61.0, tvPath: "/mnt/a/tv", moviePath: "/mnt/a/movies"},
		{name: "cold-b", dev: 102, role: model.RoleCold, total: 32 * tib, usedPct: 88.4, tvPath: "/mnt/b/tv", moviePath: "/mnt/b/movies"},
		// cold-c is already past its own 95% max - it must never be
		// picked, only drained by reclaims.
		{name: "cold-c", dev: 103, role: model.RoleCold, total: 18 * tib, usedPct: 95.7, tvPath: "/mnt/c/tv", moviePath: "/mnt/c/movies"},
		{name: "cold-d", dev: 104, role: model.RoleCold, total: 10 * tib, usedPct: 41.0, tvPath: "/mnt/d/tv", moviePath: "/mnt/d/movies"},
	}
}

// bigHotScaleDrives is the 149 TB fleet: a single 105 TB hot array
// against five cold satellites - four 10 TB drives and one 4 TB drive.
// Cold is 44 TB against 105 TB of hot, and three of the five satellites
// start past their own max, so the entire fleet has only a few TB of
// usable destination room. This is the inverted-pyramid shape where hot
// wants to shed far more than cold can ever take: the planner has to pack
// the scraps correctly, reclaim what it must, and account for the large
// remainder it cannot place, all in one pass.
func bigHotScaleDrives() []scaleDrive {
	return []scaleDrive{
		{name: "hot", dev: 200, role: model.RoleHot, total: 105 * tib, usedPct: 98.6, tvPath: "/data/tv", moviePath: "/data/movies"},
		{name: "cold-1", dev: 201, role: model.RoleCold, total: 10 * tib, usedPct: 62.0, tvPath: "/mnt/1/tv", moviePath: "/mnt/1/movies"},
		{name: "cold-2", dev: 202, role: model.RoleCold, total: 10 * tib, usedPct: 88.7, tvPath: "/mnt/2/tv", moviePath: "/mnt/2/movies"},
		// cold-3 and cold-4 are both past their 95% max already, as is
		// the small cold-5 - drainable, never fillable.
		{name: "cold-3", dev: 203, role: model.RoleCold, total: 10 * tib, usedPct: 95.7, tvPath: "/mnt/3/tv", moviePath: "/mnt/3/movies"},
		{name: "cold-4", dev: 204, role: model.RoleCold, total: 10 * tib, usedPct: 99.1, tvPath: "/mnt/4/tv", moviePath: "/mnt/4/movies"},
		{name: "cold-5", dev: 205, role: model.RoleCold, total: 4 * tib, usedPct: 99.1, tvPath: "/mnt/5/tv", moviePath: "/mnt/5/movies"},
	}
}

func scaleTiers(drives []scaleDrive) []model.Tier {
	tiers := make([]model.Tier, 0, len(drives)*2)
	for _, d := range drives {
		tv := model.Tier{Name: d.name + "-tv", Role: d.role, Paths: []string{d.tvPath}, Media: []model.MediaType{model.TV}}
		movies := model.Tier{Name: d.name + "-movies", Role: d.role, Paths: []string{d.moviePath}, Media: []model.MediaType{model.Movie}}
		if d.role == model.RoleCold {
			tv.TargetUsedPercent, tv.MaxUsedPercent = 92, 95
			movies.TargetUsedPercent, movies.MaxUsedPercent = 92, 95
		}
		tiers = append(tiers, tv, movies)
	}
	return tiers
}

// scaleUsage gives both of a drive's root folders the same usage numbers,
// which is what statfs really reports for two directories on one
// filesystem.
func scaleUsage(drives []scaleDrive) map[string]diskusage.Usage {
	out := make(map[string]diskusage.Usage, len(drives)*2)
	for _, d := range drives {
		u := usage(d.total, uint64(float64(d.total)*d.usedPct/100))
		out[d.tvPath] = u
		out[d.moviePath] = u
	}
	return out
}

func scaleVolumeOf(drives []scaleDrive) map[string]uint64 {
	out := make(map[string]uint64, len(drives)*2)
	for _, d := range drives {
		out[d.tvPath] = d.dev
		out[d.moviePath] = d.dev
	}
	return out
}

func scaleCapacity(drives []scaleDrive) uint64 {
	var total uint64
	for _, d := range drives {
		total += d.total
	}
	return total
}

// scaleRNG is a tiny deterministic generator, so the libraries below are
// byte-for-byte identical on every run and every machine. The point is a
// repeatable library of realistic shape, not statistical randomness.
type scaleRNG struct{ state uint64 }

// roll returns a value in [0, n).
func (r *scaleRNG) roll(n int64) int64 {
	r.state = r.state*6364136223846793005 + 1442695040888963407
	return int64(r.state>>33) % n //nolint:gosec // state>>33 is a 31-bit value, always exactly representable as int64
}

// scaleLibrary is the generated library plus the bookkeeping the
// assertions need: which items were made ineligible on purpose, and which
// cold-resident items are grow-risk (so a reclaim is the correct outcome
// for them and only them).
type scaleLibrary struct {
	items        []ItemEval
	inCooldown   map[model.Key]bool
	belowMinSize map[model.Key]bool
	notColdOnHot map[model.Key]bool
	growRiskCold map[model.Key]bool
}

// buildScaleLibrary fills every drive with items until roughly 92% of its
// used bytes are accounted for - the remaining ~8% stands in for content
// Coldarr doesn't manage (music, protected libraries, filesystem
// overhead), so item sizes never have to add up to a disk's used bytes
// exactly.
func buildScaleLibrary(t *testing.T, drives []scaleDrive, h *history.Store, now time.Time) scaleLibrary {
	t.Helper()

	rng := &scaleRNG{state: 20260806}
	lib := scaleLibrary{
		inCooldown:   map[model.Key]bool{},
		belowMinSize: map[model.Key]bool{},
		notColdOnHot: map[model.Key]bool{},
		growRiskCold: map[model.Key]bool{},
	}
	var nextSonarrID, nextRadarrID, coldOnHot int

	for _, d := range drives {
		budget := int64(float64(d.total)*d.usedPct/100) * 92 / 100

		for budget > 0 {
			var it model.MediaItem
			if rng.roll(2) == 0 {
				nextSonarrID++
				it = model.MediaItem{
					ArrApp: "sonarr", ID: nextSonarrID, Type: model.TV,
					Title:          fmt.Sprintf("Series %d", nextSonarrID),
					RootFolderPath: d.tvPath,
					SizeBytes:      (5 + rng.roll(115)) * gib,
				}
			} else {
				nextRadarrID++
				it = model.MediaItem{
					ArrApp: "radarr", ID: nextRadarrID, Type: model.Movie,
					Title:          fmt.Sprintf("Movie %d", nextRadarrID),
					RootFolderPath: d.moviePath,
					SizeBytes:      (2 + rng.roll(28)) * gib,
				}
			}

			var eval scoring.Evaluation
			if d.role == model.RoleHot {
				switch roll := rng.roll(100); {
				case roll < 5:
					eval = scoring.Evaluation{Decision: scoring.Protected, Reasons: []string{"marked Favorite in Jellyfin"}}
					lib.notColdOnHot[it.Key()] = true
				case roll < 30:
					eval = scoring.Evaluation{Decision: scoring.Hot, Reasons: []string{"added within the last 14 days (grace period)"}}
					lib.notColdOnHot[it.Key()] = true
				default:
					eval = scoring.Evaluation{Decision: scoring.Cold, Score: float64(40 + rng.roll(60)), Reasons: []string{"not watched recently"}}
					coldOnHot++
					switch {
					case coldOnHot%23 == 0:
						it.SizeBytes = 512 * (1 << 20) // under the 1 GB minimum move size
						lib.belowMinSize[it.Key()] = true
					case coldOnHot%17 == 0:
						if err := h.Append(history.Record{ArrApp: it.ArrApp, ItemID: it.ID, MovedAt: now.AddDate(0, 0, -3)}); err != nil {
							t.Fatalf("history.Append: %v", err)
						}
						lib.inCooldown[it.Key()] = true
					}
				}
			} else {
				switch {
				case rng.roll(100) < 2 && rng.roll(2) == 0:
					it.Upcoming = true
					eval = scoring.Evaluation{Decision: scoring.Hot, Reasons: []string{"upcoming - not yet released/premiered"}}
					lib.growRiskCold[it.Key()] = true
				case rng.roll(100) < 2:
					it.Monitored, it.QualityCutoffNotMet = true, true
					eval = scoring.Evaluation{Decision: scoring.Hot, Reasons: []string{"quality cutoff not met - expect this file to be replaced with an upgrade"}}
					lib.growRiskCold[it.Key()] = true
				default:
					eval = scoring.Evaluation{Decision: scoring.Cold, Score: float64(40 + rng.roll(60))}
				}
			}

			budget -= it.SizeBytes
			lib.items = append(lib.items, ItemEval{Item: it, Eval: eval})
		}
	}

	return lib
}

// eligible returns every item the planner should at least have
// considered: cold-scored items on hot that clear cooldown and the
// minimum move size, plus grow-risk items sitting on cold.
func (lib scaleLibrary) eligible(drives []scaleDrive, minMoveBytes int64) map[model.Key]string {
	hotPaths := map[string]bool{}
	for _, d := range drives {
		if d.role == model.RoleHot {
			hotPaths[d.tvPath] = true
			hotPaths[d.moviePath] = true
		}
	}

	out := map[model.Key]string{}
	for _, it := range lib.items {
		key := it.Item.Key()
		switch {
		case hotPaths[it.Item.RootFolderPath]:
			if it.Eval.Decision == scoring.Cold && it.Item.SizeBytes >= minMoveBytes && !lib.inCooldown[key] {
				out[key] = it.Item.Title
			}
		case lib.growRiskCold[key]:
			out[key] = it.Item.Title
		}
	}
	return out
}

// scaleResult is what checkScaleInvariants observed, for a caller to make
// its own fleet-specific assertions on top.
type scaleResult struct {
	fills    int
	reclaims int
	phases   int
	// peak is the highest used-percent each path reached at any point
	// during the plan, not just where it ended. Interleaved reclaims can
	// pull items back off cold before later fills use the room, so peak is
	// what shows how tightly each drive was actually filled.
	peak map[string]float64
	// leftovers are eligible candidates the plan could not place.
	leftovers map[model.Key]string
	// firstFillDest is the path the very first hot->cold move targeted.
	firstFillDest string
}

func scaleCopyUsage(usageMap map[string]diskusage.Usage) map[string]diskusage.Usage {
	out := make(map[string]diskusage.Usage, len(usageMap))
	for path, u := range usageMap {
		out[path] = u
	}
	return out
}

// scaleApplyDelta is intentionally independent of planner.applyDelta. The
// scale replay is meant to catch planner bookkeeping defects, so calling the
// implementation under test here would let the same bug agree with itself.
func scaleApplyDelta(u diskusage.Usage, deltaBytes int64) diskusage.Usage {
	used := int64(u.UsedBytes) + deltaBytes //nolint:gosec // fixture sizes stay well inside int64
	if used < 0 {
		used = 0
	}
	free := int64(u.FreeBytes) - deltaBytes //nolint:gosec // fixture sizes stay well inside int64
	if free < 0 {
		free = 0
	}
	u.UsedBytes = uint64(used)
	u.FreeBytes = uint64(free)
	denom := u.UsedBytes + u.FreeBytes
	if denom == 0 {
		u.UsedPercent = 0
	} else {
		u.UsedPercent = float64(u.UsedBytes) / float64(denom) * 100
	}
	return u
}

// scaleApplyDeltaToDevice independently mirrors the fixture's device map.
// Paths absent from volumeOf are standalone; paths on the same device are
// updated together because statfs would report the same capacity for each.
func scaleApplyDeltaToDevice(usageMap map[string]diskusage.Usage, volumeOf map[string]uint64, path string, deltaBytes int64) {
	device, shared := volumeOf[path]
	if !shared {
		if u, ok := usageMap[path]; ok {
			usageMap[path] = scaleApplyDelta(u, deltaBytes)
		}
		return
	}
	for candidate, candidateDevice := range volumeOf {
		if candidateDevice != device {
			continue
		}
		if u, ok := usageMap[candidate]; ok {
			usageMap[candidate] = scaleApplyDelta(u, deltaBytes)
		}
	}
}

// checkScaleInvariants asserts everything that must hold for any fleet,
// whatever its shape: legal moves only, no double moves, ceilings
// respected at the moment of every placement, shared-volume capacity
// never spent twice, reclaims ordered after the fills that make room for
// them, a plan shallow enough for the mover to still overlap drives, and
// every eligible item accounted for either in the plan or in a warning.
// maxPhases is the caller's budget for how deep the fill/reclaim
// dependency chain is allowed to get on that fleet - see the phase check
// below for why it is a per-fleet number rather than a constant.
func checkScaleInvariants(t *testing.T, drives []scaleDrive, tiers []model.Tier, lib scaleLibrary, in Input, plan *Plan, maxPhases int) scaleResult {
	t.Helper()

	startUsage := scaleUsage(drives)
	tierOfPath := map[string]model.Tier{}
	for _, tier := range tiers {
		for _, p := range tier.Paths {
			tierOfPath[p] = tier
		}
	}

	// Build must not have mutated its input - the caller still needs the
	// pre-plan numbers to report before/after usage.
	for path, u := range startUsage {
		if in.Usage[path] != u {
			t.Fatalf("Build mutated Input.Usage for %s: %+v vs %+v", path, in.Usage[path], u)
		}
	}

	res := scaleResult{peak: make(map[string]float64, len(startUsage))}
	leftovers := lib.eligible(drives, int64(in.Policy.MinMoveSizeGB*(1<<30)))

	// Every move must be a legal one for that specific item, and no item
	// may be moved twice in a single plan.
	planned := map[model.Key]string{}
	firstFillPhase := -1
	firstReclaimPhase := -1
	fillAfterReclaim := false
	for _, e := range plan.Entries {
		key := e.Item.Key()
		if _, dup := planned[key]; dup {
			t.Fatalf("item %q planned for two moves in one plan", e.Item.Title)
		}
		if _, ok := leftovers[key]; !ok {
			t.Fatalf("planned a move for %q, which was never an eligible candidate", e.Item.Title)
		}
		planned[key] = e.Item.Title
		delete(leftovers, key)

		dst, ok := tierOfPath[e.ToPath]
		if !ok {
			t.Fatalf("move %q targets unknown path %s", e.Item.Title, e.ToPath)
		}
		if !dst.AcceptsMediaType(e.Item.Type) {
			t.Fatalf("move %q (%s) targets %s, which does not accept that media type", e.Item.Title, e.Item.Type, dst.Name)
		}
		src, ok := tierOfPath[e.FromPath]
		if !ok {
			t.Fatalf("move %q originates from unknown path %s", e.Item.Title, e.FromPath)
		}
		if src.Role == dst.Role {
			t.Fatalf("move %q goes %s->%s - neither a fill nor a reclaim: %+v", e.Item.Title, src.Role, dst.Role, e)
		}

		switch dst.Role {
		case model.RoleCold:
			res.fills++
			if firstReclaimPhase >= 0 {
				fillAfterReclaim = true
			}
			if firstFillPhase < 0 || e.Phase < firstFillPhase {
				firstFillPhase = e.Phase
			}
			if res.firstFillDest == "" {
				res.firstFillDest = e.ToPath
			}
			if lib.notColdOnHot[key] {
				t.Fatalf("item %q is not cold-scored but was planned for cold storage", e.Item.Title)
			}
			if lib.inCooldown[key] {
				t.Fatalf("item %q is in cooldown but was planned for a move", e.Item.Title)
			}
			if lib.belowMinSize[key] {
				t.Fatalf("item %q is below the minimum move size but was planned for a move", e.Item.Title)
			}
		case model.RoleHot:
			res.reclaims++
			if firstReclaimPhase < 0 {
				firstReclaimPhase = e.Phase
			}
			if !lib.growRiskCold[key] {
				t.Fatalf("item %q was reclaimed to hot but is not upcoming or cutoff-unmet", e.Item.Title)
			}
		}
		if e.MaxUsedPercent != dst.EffectiveMaxUsedPercent() {
			t.Fatalf("move %q records destination ceiling %.2f, want effective %.2f", e.Item.Title, e.MaxUsedPercent, dst.EffectiveMaxUsedPercent())
		}
	}
	res.leftovers = leftovers
	if res.fills == 0 || res.reclaims == 0 {
		t.Fatalf("expected the scenario to produce both fills and reclaims, got %d fills and %d reclaims", res.fills, res.reclaims)
	}

	// Hot starts above its ceiling in every scale fleet, so the first
	// reclaim genuinely needs an earlier fill phase. Once enough room is
	// available, however, reclaims should run before unrelated remaining
	// fills instead of being bunched at the end of the plan.
	if firstReclaimPhase <= firstFillPhase {
		t.Fatalf("first reclaim phase %d is not after its enabling fill phase %d", firstReclaimPhase, firstFillPhase)
	}
	if !fillAfterReclaim {
		t.Fatal("all reclaims were delayed until after every fill; expected unrelated fills after the first reclaim batch")
	}

	// The mover runs phases strictly one after another, waiting for each
	// to fully land - settle included - before starting the next, so
	// phase count is a hard cap on how much of a plan can overlap across
	// drives. How deep the chain gets is a property of the fleet, not of
	// the library size: when cold has room, one round of fills is enough
	// and everything resolves in two or three phases. When cold is nearly
	// full, fills and reclaims feed each other - fills free hot room, the
	// reclaims that unlocks free cold room, which unlocks more fills -
	// and the chain runs as many rounds deep as that ping-pong lasts.
	// Each caller passes what its fleet should need; the point is that it
	// stays bounded by the real dependency chain and never approaches the
	// candidate count.
	phases := map[int]bool{}
	lastPhase := -1
	for _, e := range plan.Entries {
		if e.Phase < lastPhase {
			t.Fatalf("entries are not phase-monotone: phase %d appears after phase %d", e.Phase, lastPhase)
		}
		lastPhase = e.Phase
		phases[e.Phase] = true
	}
	res.phases = len(phases)
	if res.phases > maxPhases {
		t.Errorf("plan fragmented into %d sequential mover phases (budget %d); the mover would serialize that much of the run", res.phases, maxPhases)
	}

	// Replay at actual phase boundaries. Every destination in a phase is
	// checked against the phase's starting usage plus other destination
	// reservations, never against source bytes another concurrent move only
	// promises to free. This independently catches unsafe same-phase credit
	// as well as aggregate ceiling and writable-space overshoots.
	replay := scaleCopyUsage(startUsage)
	for k, v := range startUsage {
		res.peak[k] = v.UsedPercent
	}
	for phaseStart := 0; phaseStart < len(plan.Entries); {
		phase := plan.Entries[phaseStart].Phase
		phaseEnd := phaseStart + 1
		for phaseEnd < len(plan.Entries) && plan.Entries[phaseEnd].Phase == phase {
			phaseEnd++
		}

		capacity := scaleCopyUsage(replay)
		for _, e := range plan.Entries[phaseStart:phaseEnd] {
			before, ok := capacity[e.ToPath]
			if !ok {
				t.Fatalf("phase %d move %q targets unavailable path %s", phase, e.Item.Title, e.ToPath)
			}
			if e.Item.SizeBytes < 0 || uint64(e.Item.SizeBytes) > before.FreeBytes {
				t.Fatalf("phase %d move %q needs %.1f GB at %s but only %.1f GB is writable at the phase boundary", phase, e.Item.Title, gb(e.Item.SizeBytes), e.ToPath, float64(before.FreeBytes)/gib)
			}
			projected := scaleApplyDelta(before, e.Item.SizeBytes)
			if projected.UsedPercent > e.MaxUsedPercent {
				t.Fatalf("phase %d move %q (%.1f GB) to %s would take it to %.2f%% used, past its %.2f%% ceiling", phase, e.Item.Title, gb(e.Item.SizeBytes), e.ToPath, projected.UsedPercent, e.MaxUsedPercent)
			}
			if projected.UsedBytes > before.TotalBytes {
				t.Fatalf("phase %d move %q to %s would use %.1f TB of a %.1f TB disk", phase, e.Item.Title, e.ToPath, float64(projected.UsedBytes)/tib, float64(before.TotalBytes)/tib)
			}
			// Reserve only writes while validating this concurrent phase.
			scaleApplyDeltaToDevice(capacity, in.VolumeOf, e.ToPath, e.Item.SizeBytes)
		}

		// Once the whole phase has settled, both sides become visible to the
		// next phase.
		for _, e := range plan.Entries[phaseStart:phaseEnd] {
			scaleApplyDeltaToDevice(replay, in.VolumeOf, e.FromPath, -e.Item.SizeBytes)
			scaleApplyDeltaToDevice(replay, in.VolumeOf, e.ToPath, e.Item.SizeBytes)
		}
		for p, u := range replay {
			if u.UsedPercent > res.peak[p] {
				res.peak[p] = u.UsedPercent
			}
		}
		phaseStart = phaseEnd
	}

	// The two root folders on a drive are one pool of bytes: they must
	// stay in lockstep, and the planner's own projection must agree with
	// an independent replay. Disagreement here is free space spent twice.
	for _, d := range drives {
		tv, movies := plan.FinalUsage[d.tvPath], plan.FinalUsage[d.moviePath]
		if tv != movies {
			t.Errorf("%s: the two root folders on one disk projected different usage: %+v vs %+v", d.name, tv, movies)
		}
		if tv != replay[d.tvPath] {
			t.Errorf("%s: FinalUsage disagrees with an independent replay of the plan: %+v vs %+v", d.name, tv, replay[d.tvPath])
		}
	}

	// Whatever could not be placed has to be named in a warning - one per
	// leftover, no more (a per-round warning would report the same item
	// several times, including ones that later succeeded). With hundreds
	// of candidates retried across rounds, the failure mode that matters
	// is the silent one: an item dropped between rounds that nobody
	// notices is missing from a 300-move plan.
	if len(plan.Warnings) != len(leftovers) {
		t.Errorf("expected exactly one warning per unplaced candidate: %d warnings for %d leftovers", len(plan.Warnings), len(leftovers))
	}
	warned := map[string]bool{}
	for _, w := range plan.Warnings {
		warned[w] = true
	}
	missing := 0
	for _, title := range leftovers {
		found := false
		for w := range warned {
			if strings.Contains(w, fmt.Sprintf("%q", title)) {
				found = true
				break
			}
		}
		if !found {
			missing++
			if missing <= 3 {
				t.Errorf("candidate %q was neither planned nor warned about - it vanished between rounds", title)
			}
		}
	}
	if missing > 3 {
		t.Errorf("%d candidates in total vanished without a plan entry or a warning", missing)
	}
	for w := range warned {
		for _, title := range planned {
			if strings.Contains(w, fmt.Sprintf("%q", title)) {
				t.Errorf("item %q was planned but also warned about: %s", title, w)
			}
		}
	}

	return res
}

// checkPacksFullestDriveFirst asserts the plan opened the cold drive that
// was already fullest while still under target, rather than spreading the
// first moves over emptier ones. Packing satellites one at a time is the
// whole point of preferring the fullest viable destination.
func checkPacksFullestDriveFirst(t *testing.T, drives []scaleDrive, res scaleResult) {
	t.Helper()

	var want scaleDrive
	for _, d := range drives {
		if d.role != model.RoleCold || d.usedPct > 92 {
			continue
		}
		if want.name == "" || d.usedPct > want.usedPct {
			want = d
		}
	}
	if want.name == "" {
		t.Fatal("fleet has no cold drive under target - nothing could ever be filled")
	}
	if res.firstFillDest != want.tvPath && res.firstFillDest != want.moviePath {
		t.Errorf("first fill went to %s; expected %s (%.1f%%, the fullest drive still under target)", res.firstFillDest, want.name, want.usedPct)
	}
	if res.peak[want.tvPath] < 91.5 {
		t.Errorf("expected %s to be packed to its 92%% target, peaked at %.2f%%", want.name, res.peak[want.tvPath])
	}
}

// checkNeverFillsDrivesPastMax asserts drives that started past their own
// max are only ever drained, never picked as a destination.
func checkNeverFillsDrivesPastMax(t *testing.T, drives []scaleDrive, res scaleResult) {
	t.Helper()

	for _, d := range drives {
		if d.role != model.RoleCold || d.usedPct <= 95 {
			continue
		}
		if res.peak[d.tvPath] > d.usedPct+0.0001 || res.peak[d.moviePath] > d.usedPct+0.0001 {
			t.Errorf("%s started at %.1f%%, past its 95%% max, but was filled to %.2f%%", d.name, d.usedPct, res.peak[d.tvPath])
		}
	}
}

func planScaleFleet(t *testing.T, drives []scaleDrive) (scaleLibrary, Input, *Plan) {
	t.Helper()

	now := time.Now()
	h := emptyHistory(t)
	lib := buildScaleLibrary(t, drives, h, now)

	in := Input{
		Tiers:    scaleTiers(drives),
		Usage:    scaleUsage(drives),
		VolumeOf: scaleVolumeOf(drives),
		Items:    lib.items,
		History:  h,
		Policy:   config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:      now,
	}

	started := time.Now()
	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Logf("planned %d moves over %d items / %.0f TB in %s", len(plan.Entries), len(lib.items), float64(scaleCapacity(drives))/tib, time.Since(started).Round(time.Millisecond))

	return lib, in, plan
}

// TestBuild_LargeLibraryAcrossFourColdDrivesAndOneHot runs a full plan
// over 120 TB of storage - one 20 TB hot drive and four cold drives of
// four different sizes - holding a 2,000+ item library, where cold has
// room to spare.
func TestBuild_LargeLibraryAcrossFourColdDrivesAndOneHot(t *testing.T) {
	drives := scaleDrives()
	tiers := scaleTiers(drives)

	if got := scaleCapacity(drives); got != 120*tib {
		t.Fatalf("scenario should model 120 TB of storage, got %.1f TB", float64(got)/tib)
	}

	lib, in, plan := planScaleFleet(t, drives)
	if len(lib.items) < 2000 {
		t.Fatalf("scenario should model a 2,000+ item library, got %d items", len(lib.items))
	}

	// Cold has room to spare. The first newly enabled reclaim runs eagerly;
	// subsequent goals grow 4x, so all 349 moves still fit in a single-digit
	// number of phases rather than one phase per reclaim.
	res := checkScaleInvariants(t, drives, tiers, lib, in, plan, 10)
	checkPacksFullestDriveFirst(t, drives, res)
	checkNeverFillsDrivesPastMax(t, drives, res)

	// Packing concentrates: cold-b (88.4%, fullest under target) is
	// filled to target first, cold-a (61%) takes the overflow, and the
	// 10 TB cold-d is never opened while cold-a still has target room.
	if res.peak["/mnt/a/tv"] > 92.0001 {
		t.Errorf("cold-a peaked at %.2f%%, past its 92%% target while it still had target room", res.peak["/mnt/a/tv"])
	}
	if res.peak["/mnt/d/tv"] > 41.0001 && res.peak["/mnt/a/tv"] < 91.5 {
		t.Errorf("cold-d (%.2f%%) was opened while cold-a (%.2f%%) still had target room - packing should concentrate, not spread", res.peak["/mnt/d/tv"], res.peak["/mnt/a/tv"])
	}

	// Cold has room to spare here, so the library should almost entirely
	// find a home - the opposite of the big-hot fleet below.
	if len(res.leftovers) != 0 {
		t.Errorf("expected cold to absorb everything eligible, %d candidates left over", len(res.leftovers))
	}

	t.Logf("fills=%d reclaims=%d phases=%d leftovers=%d; hot %.2f%% -> %.2f%%; cold peak a=%.2f%% b=%.2f%% c=%.2f%% d=%.2f%%",
		res.fills, res.reclaims, res.phases, len(res.leftovers),
		in.Usage["/data/tv"].UsedPercent, plan.FinalUsage["/data/tv"].UsedPercent,
		res.peak["/mnt/a/tv"], res.peak["/mnt/b/tv"], res.peak["/mnt/c/tv"], res.peak["/mnt/d/tv"])
}

// TestBuild_LargeLibraryOnBigHotWithSmallColdSatellites runs the same
// checks over the inverted fleet: 149 TB total, but as a 105 TB hot array
// against five small cold satellites (4x10 TB + 4 TB), three of which
// already sit past their own max. Cold can take only a few TB of the tens
// of TB hot wants to shed, so almost every invariant is exercised under
// scarcity instead of plenty: the scraps of room have to be packed
// correctly, the reclaims still have to happen, and the very large
// remainder has to be reported rather than silently dropped.
func TestBuild_LargeLibraryOnBigHotWithSmallColdSatellites(t *testing.T) {
	drives := bigHotScaleDrives()
	tiers := scaleTiers(drives)

	if got := scaleCapacity(drives); got != 149*tib {
		t.Fatalf("scenario should model 149 TB of storage (105 hot + 4x10 + 4 cold), got %.1f TB", float64(got)/tib)
	}
	var hotTotal, coldTotal uint64
	for _, d := range drives {
		if d.role == model.RoleHot {
			hotTotal += d.total
			continue
		}
		coldTotal += d.total
	}
	if hotTotal != 105*tib || coldTotal != 44*tib {
		t.Fatalf("expected a 105 TB hot array against 44 TB of cold, got %.1f TB hot / %.1f TB cold", float64(hotTotal)/tib, float64(coldTotal)/tib)
	}

	lib, in, plan := planScaleFleet(t, drives)
	if len(lib.items) < 2000 {
		t.Fatalf("scenario should model a 2,000+ item library, got %d items", len(lib.items))
	}

	// The first reclaim runs at its earliest safe boundary; 4x batching
	// keeps the rest of the chain in single digits despite the scarcity.
	res := checkScaleInvariants(t, drives, tiers, lib, in, plan, 10)
	checkPacksFullestDriveFirst(t, drives, res)
	checkNeverFillsDrivesPastMax(t, drives, res)

	// Only cold-1 (62%) and cold-2 (88.7%) can take anything at all, and
	// between them that is a few TB against a 105 TB hot array - so the
	// plan must be mostly leftovers, and both usable drives should end up
	// packed to their 95% max rather than stopping at target while
	// candidates go unplaced.
	if len(res.leftovers) < len(lib.items)/4 {
		t.Errorf("expected most of the library to have nowhere to go on 44 TB of mostly-full cold, only %d of %d left over", len(res.leftovers), len(lib.items))
	}
	for _, path := range []string{"/mnt/1/tv", "/mnt/2/tv"} {
		if res.peak[path] < 94.5 {
			t.Errorf("%s peaked at %.2f%%; with candidates still unplaced it should have been packed to its 95%% max via the fallback pass", path, res.peak[path])
		}
	}

	// The reclaims are the point of this fleet: hot is 98.6% full and
	// cold is nearly untouchable, yet grow-risk items still have to come
	// back off cold - the few TB of fills must be enough to get hot under
	// its ceiling first.
	hotAfter := plan.FinalUsage["/data/tv"].UsedPercent
	if hotAfter > model.DefaultHotMaxUsedPercent {
		t.Errorf("hot ended at %.2f%%, above its %.1f%% ceiling", hotAfter, model.DefaultHotMaxUsedPercent)
	}

	t.Logf("fills=%d reclaims=%d phases=%d leftovers=%d; hot %.2f%% -> %.2f%%; cold peak 1=%.2f%% 2=%.2f%% 3=%.2f%% 4=%.2f%% 5=%.2f%%",
		res.fills, res.reclaims, res.phases, len(res.leftovers),
		in.Usage["/data/tv"].UsedPercent, hotAfter,
		res.peak["/mnt/1/tv"], res.peak["/mnt/2/tv"], res.peak["/mnt/3/tv"], res.peak["/mnt/4/tv"], res.peak["/mnt/5/tv"])
}

// TestBuild_LargeLibraryReportsEveryItemItCannotPlace runs the 120 TB
// fleet with every cold drive pushed to or past its target, so the only
// room left anywhere is the sliver between target and max. It is the
// scarcity case for a fleet whose drives are all large: unlike the
// big-hot fleet, no single drive is roomy, so the fallback-to-max pass
// has to do all the work.
func TestBuild_LargeLibraryReportsEveryItemItCannotPlace(t *testing.T) {
	drives := scaleDrives()
	for i := range drives {
		switch drives[i].name {
		case "cold-a", "cold-d":
			drives[i].usedPct = 94.0 // past its 92% target, ~1% of max-fallback room left
		case "cold-b":
			drives[i].usedPct = 94.5
		}
	}
	tiers := scaleTiers(drives)

	lib, in, plan := planScaleFleet(t, drives)
	// This is the fleet that makes fills and reclaims feed each other:
	// with no drive holding more than a sliver of room, each handful of
	// fills unlocks a handful of reclaims, which frees cold room for the
	// next handful of fills. That is a real dependency chain - the mover
	// genuinely cannot run these in parallel - but it costs a full
	// settle-and-wait per wave, so it must stay in the low tens rather
	// than growing with the 363 candidates competing here. The 36-phase
	// ceiling still requires each phase to resolve many candidates on
	// average, while allowing the real fill/reclaim dependency waves.
	res := checkScaleInvariants(t, drives, tiers, lib, in, plan, 36)
	checkNeverFillsDrivesPastMax(t, drives, res)

	if res.fills+res.reclaims+len(res.leftovers) < 300 {
		t.Fatalf("expected hundreds of candidates competing for scraps, got %d", res.fills+res.reclaims+len(res.leftovers))
	}
	if len(res.leftovers) == 0 {
		t.Fatal("expected far more candidates than room, but everything was placed")
	}

	t.Logf("tight fleet: %d candidates, %d placed over %d phases, %d left over",
		res.fills+res.reclaims+len(res.leftovers), res.fills+res.reclaims, res.phases, len(res.leftovers))
}
