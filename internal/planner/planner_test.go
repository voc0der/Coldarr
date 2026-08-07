package planner

import (
	"fmt"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/scoring"
)

const (
	gib = 1 << 30
	tib = 1 << 40
)

func usage(total, used uint64) diskusage.Usage {
	return diskusage.Usage{
		TotalBytes:  total,
		UsedBytes:   used,
		FreeBytes:   total - used,
		UsedPercent: float64(used) / float64(total) * 100,
	}
}

func testTiers() []model.Tier {
	return []model.Tier{
		{
			Name:  "hot",
			Role:  model.RoleHot,
			Paths: []string{"/hot"},
			Media: []model.MediaType{model.Movie, model.TV},
		},
		{
			Name:              "cold-movies",
			Role:              model.RoleCold,
			Paths:             []string{"/cold1", "/cold2"},
			Media:             []model.MediaType{model.Movie},
			TargetUsedPercent: 92,
			MaxUsedPercent:    95,
		},
	}
}

func emptyHistory(t *testing.T) *history.Store {
	t.Helper()
	h, err := history.Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}
	return h
}

func coldItem(id int, title string, sizeBytes int64) ItemEval {
	return ItemEval{
		Item: model.MediaItem{
			ArrApp:         "radarr",
			ID:             id,
			Type:           model.Movie,
			Title:          title,
			RootFolderPath: "/hot",
			SizeBytes:      sizeBytes,
		},
		Eval: scoring.Evaluation{Decision: scoring.Cold, Score: float64(50 + id)},
	}
}

func TestBuild_NoMoveWhenNoColdCandidates(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 950*gib), // hot near-full is fine, not a trigger
			"/cold1": usage(1000*gib, 100*gib),
			"/cold2": usage(1000*gib, 100*gib),
		},
		Items: []ItemEval{
			{
				Item: model.MediaItem{ArrApp: "radarr", ID: 1, Type: model.Movie, Title: "Hot Movie", RootFolderPath: "/hot", SizeBytes: 20 * gib},
				Eval: scoring.Evaluation{Decision: scoring.Hot},
			},
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected no moves, got %d", len(plan.Entries))
	}
}

func TestBuild_MovesColdItemsRegardlessOfHotUsage(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib), // hot barely used - old model would never trigger here
			"/cold1": usage(1000*gib, 100*gib),
			"/cold2": usage(1000*gib, 100*gib),
		},
		Items: []ItemEval{
			coldItem(1, "Movie A", 60*gib),
			coldItem(2, "Movie B", 60*gib),
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Every cold-eligible item on hot storage moves whenever cold has
	// room - there's no hot-side pressure gate anymore.
	if len(plan.Entries) != 2 {
		t.Fatalf("expected 2 moves, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	if plan.Entries[0].Item.Title != "Movie B" {
		t.Fatalf("expected coldest item first, got %q", plan.Entries[0].Item.Title)
	}
	for _, e := range plan.Entries {
		if e.FromPath != "/hot" || e.ToTier != "cold-movies" {
			t.Fatalf("unexpected move: %+v", e)
		}
	}
}

func TestBuild_PrefersTargetOverMaxFallback(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 500*gib),
			"/cold1": usage(100*gib, 50*gib), // well under its 92% target
			"/cold2": usage(100*gib, 93*gib), // over target, but still under its 95% max
		},
		Items: []ItemEval{
			coldItem(1, "Movie A", 1*gib),
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 0.5},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected 1 move, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	// /cold2 would also technically fit under its max, but /cold1 has
	// room under target - target-eligible destinations always win over a
	// max-only fallback, regardless of relative fullness.
	if plan.Entries[0].ToPath != "/cold1" {
		t.Fatalf("expected destination /cold1 (has target room), got %s", plan.Entries[0].ToPath)
	}
}

func TestBuild_FallsBackToMaxWhenNoTargetRoomExists(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 500*gib),
			"/cold1": usage(100*gib, 93*gib), // past its 92% target, 2 GB of room left under its 95% max
			"/cold2": usage(100*gib, 94*gib), // past its 92% target, only 1 GB of room left under its 95% max
		},
		Items: []ItemEval{
			coldItem(1, "Movie A", 1500*(1<<20)), // 1.5 GB - fits /cold1's max headroom but not /cold2's
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected 1 move, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	if plan.Entries[0].ToPath != "/cold1" {
		t.Fatalf("expected destination /cold1 (only one with max headroom), got %s", plan.Entries[0].ToPath)
	}
	final := plan.FinalUsage["/cold1"]
	if final.UsedPercent > 95 {
		t.Fatalf("destination exceeded its max ceiling: %.2f%%", final.UsedPercent)
	}
}

func TestBuild_NeverExceedsMaxEvenAsFallback(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 500*gib),
			"/cold1": usage(100*gib, 94*gib), // almost at its 95% max already
			"/cold2": usage(100*gib, 94*gib),
		},
		Items: []ItemEval{
			coldItem(1, "Big Movie", 50*gib), // would push either past 95% max
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected no moves when every destination would exceed its max, got %d", len(plan.Entries))
	}
	if len(plan.Warnings) == 0 {
		t.Fatalf("expected a warning that no destination has room")
	}
}

func TestBuild_RespectsCooldown(t *testing.T) {
	h := emptyHistory(t)
	item := coldItem(1, "Movie A", 60*gib)
	if err := h.Append(history.Record{
		ArrApp:  item.Item.ArrApp,
		ItemID:  item.Item.ID,
		MovedAt: time.Now().AddDate(0, 0, -5), // moved 5 days ago
	}); err != nil {
		t.Fatalf("history.Append: %v", err)
	}

	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 500*gib),
			"/cold1": usage(1000*gib, 100*gib),
			"/cold2": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{item},
		History: h,
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1}, // 30-day cooldown, moved 5 days ago
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected item in cooldown to be skipped, got %d moves", len(plan.Entries))
	}
}

func TestBuild_TreatsSameVolumePathsAsSharedCapacity(t *testing.T) {
	tiers := []model.Tier{
		{
			Name:  "hot",
			Role:  model.RoleHot,
			Paths: []string{"/hot"},
			Media: []model.MediaType{model.Movie},
		},
		{
			Name:              "cold-a",
			Role:              model.RoleCold,
			Paths:             []string{"/cold1"},
			Media:             []model.MediaType{model.Movie},
			TargetUsedPercent: 92,
			MaxUsedPercent:    95,
		},
		{
			Name:              "cold-b",
			Role:              model.RoleCold,
			Paths:             []string{"/cold-shared"},
			Media:             []model.MediaType{model.Movie},
			TargetUsedPercent: 92,
			MaxUsedPercent:    95,
		},
	}

	in := Input{
		Tiers: tiers,
		Usage: map[string]diskusage.Usage{
			"/hot":         usage(1000*gib, 500*gib),
			"/cold1":       usage(100*gib, 50*gib),
			"/cold-shared": usage(100*gib, 50*gib),
		},
		// /cold1 and /cold-shared are different tiers/paths but the same
		// physical volume - moving into one must reduce the other's room
		// too, or the planner would double-count 50 GB of free space as
		// 100 GB.
		VolumeOf: map[string]uint64{
			"/cold1":       42,
			"/cold-shared": 42,
		},
		Items: []ItemEval{
			coldItem(1, "Movie A", 40*gib),
			coldItem(2, "Movie B", 40*gib),
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// The shared disk only has 50 GB free - enough for one 40 GB item,
	// not two, even though each path's tier looks independently roomy.
	if len(plan.Entries) != 1 {
		t.Fatalf("expected exactly 1 move (shared volume only has room for one), got %d: %+v", len(plan.Entries), plan.Entries)
	}
	if len(plan.Warnings) == 0 {
		t.Fatalf("expected a warning that the second item found no room")
	}

	final := plan.FinalUsage["/cold1"]
	finalShared := plan.FinalUsage["/cold-shared"]
	if final.UsedPercent > 95 {
		t.Fatalf("/cold1 exceeded its max: %.2f%%", final.UsedPercent)
	}
	if final.UsedPercent != finalShared.UsedPercent {
		t.Fatalf("expected /cold1 and /cold-shared (same volume) to report identical usage, got %.2f%% vs %.2f%%", final.UsedPercent, finalShared.UsedPercent)
	}
}

func TestBuild_UnavailablePathIsSkippedNotAssumedEmpty(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot": usage(1000*gib, 500*gib),
			// /cold1 and /cold2 deliberately absent, simulating failed
			// mount checks.
		},
		Items:   []ItemEval{coldItem(1, "Movie A", 60*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected no moves when all destinations are unavailable, got %d", len(plan.Entries))
	}
	if len(plan.Warnings) == 0 {
		t.Fatalf("expected a warning about no destination having room")
	}
}

func TestBuild_UnavailableSourcePathsAreNotPlanned(t *testing.T) {
	t.Run("fill source", func(t *testing.T) {
		in := Input{
			Tiers: testTiers(),
			Usage: map[string]diskusage.Usage{
				// /hot is deliberately absent.
				"/cold1": usage(1000*gib, 100*gib),
				"/cold2": usage(1000*gib, 100*gib),
			},
			Items:   []ItemEval{coldItem(1, "Movie on unavailable hot", 5*gib)},
			History: emptyHistory(t),
			Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
			Now:     time.Now(),
		}

		plan, err := Build(in)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(plan.Entries) != 0 {
			t.Fatalf("must not plan from an unavailable source: %+v", plan.Entries)
		}
	})

	t.Run("reclaim source", func(t *testing.T) {
		in := Input{
			Tiers: tvTiers(),
			Usage: map[string]diskusage.Usage{
				"/hot": usage(1000*gib, 100*gib),
				// /cold1 is deliberately absent.
			},
			Items:   []ItemEval{upcomingItem("Show on unavailable cold", "/cold1", 5*gib)},
			History: emptyHistory(t),
			Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
			Now:     time.Now(),
		}

		plan, err := Build(in)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if len(plan.Entries) != 0 {
			t.Fatalf("must not plan from an unavailable source: %+v", plan.Entries)
		}
	})
}

func TestBuild_SamePhaseDoesNotSpendAnotherMovesSourceFree(t *testing.T) {
	tiers := []model.Tier{
		{Name: "hot-tv", Role: model.RoleHot, Paths: []string{"/hot-tv"}, Media: []model.MediaType{model.TV}},
		{Name: "hot-movies", Role: model.RoleHot, Paths: []string{"/hot-movies"}, Media: []model.MediaType{model.Movie}},
		{Name: "cold-tv", Role: model.RoleCold, Paths: []string{"/cold-tv"}, Media: []model.MediaType{model.TV}, TargetUsedPercent: 100, MaxUsedPercent: 100},
		{Name: "cold-movies", Role: model.RoleCold, Paths: []string{"/cold-movies"}, Media: []model.MediaType{model.Movie}, TargetUsedPercent: 100, MaxUsedPercent: 100},
	}
	movie := ItemEval{
		Item: model.MediaItem{ArrApp: "radarr", ID: 1, Type: model.Movie, Title: "Movie frees shared volume", RootFolderPath: "/hot-movies", SizeBytes: 10 * gib},
		Eval: scoring.Evaluation{Decision: scoring.Cold, Score: 100},
	}
	show := ItemEval{
		Item: model.MediaItem{ArrApp: "sonarr", ID: 1, Type: model.TV, Title: "Show needs freed space", RootFolderPath: "/hot-tv", SizeBytes: 10 * gib},
		Eval: scoring.Evaluation{Decision: scoring.Cold, Score: 90},
	}
	shared := usage(100*gib, 95*gib)
	in := Input{
		Tiers: tiers,
		Usage: map[string]diskusage.Usage{
			"/hot-tv":      usage(100*gib, 50*gib),
			"/hot-movies":  shared,
			"/cold-tv":     shared, // same device as /hot-movies: only 5 GB free initially
			"/cold-movies": usage(100*gib, 10*gib),
		},
		VolumeOf: map[string]uint64{
			"/hot-movies": 2,
			"/cold-tv":    2,
		},
		Items:   []ItemEval{movie, show},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 2 {
		t.Fatalf("both fills should land after the dependency is phased safely: %+v", plan.Entries)
	}
	phaseByTitle := map[string]int{}
	for _, entry := range plan.Entries {
		phaseByTitle[entry.Item.Title] = entry.Phase
	}
	if phaseByTitle[movie.Item.Title] >= phaseByTitle[show.Item.Title] {
		t.Fatalf("show must wait for the prior phase to really free its destination volume, phases=%v", phaseByTitle)
	}
}

// TestBuild_RespectsRealFreeSpaceNotRawTotal reproduces a real incident: a
// filesystem with a reserved-blocks margin (e.g. ext4's default 5%
// root-reserved blocks) reports FreeBytes (Bavail-based, what a writer can
// actually use) smaller than TotalBytes-UsedBytes would suggest. If the
// ceiling check divides by raw TotalBytes instead of Used+Free, it can
// accept an item that looks like it fits by nominal-capacity math but
// doesn't actually fit in the space a writer can reach - which is exactly
// what let a prior plan overshoot its configured max_used_percent and run
// a cold drive to `df -h` 100% while Coldarr's own projection still showed
// headroom.
func TestBuild_RespectsRealFreeSpaceNotRawTotal(t *testing.T) {
	tiers := testTiers()
	tiers[1].Paths = []string{"/cold1"}
	// A 100% policy ceiling deliberately removes percentage as the reason
	// to reject this move. Only the writer-visible FreeBytes check can do it.
	tiers[1].MaxUsedPercent = 100
	tiers[1].TargetUsedPercent = 100
	in := Input{
		Tiers: tiers,
		Usage: map[string]diskusage.Usage{
			"/hot": usage(1000*gib, 500*gib),
			// Raw total says 10 GB "free" (100-90), but only 5 GB of that
			// is actually writable (Bavail) - 5 GB is reserved and
			// invisible to df's Use% and to any unprivileged writer.
			"/cold1": {TotalBytes: 100 * gib, UsedBytes: 90 * gib, FreeBytes: 5 * gib, UsedPercent: 90.0 / 95.0 * 100},
		},
		Items: []ItemEval{
			// Bigger than the real 5 GB free. applyDelta clamps projected
			// free to zero, so without a direct check this appears to land
			// at exactly the otherwise-permitted 100% ceiling.
			coldItem(1, "Movie A", 6*gib),
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}
	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected /cold1 to be rejected (only 5 GB actually free for a 6 GB item), got: %+v", plan.Entries)
	}
	if len(plan.Warnings) != 1 {
		t.Fatalf("expected the unplaceable item to be reported once, got %v", plan.Warnings)
	}
}

// tvTiers is like testTiers but with a cold tier that accepts TV, for the
// upcoming-on-cold promotion tests below.
func tvTiers() []model.Tier {
	return []model.Tier{
		{Name: "hot", Role: model.RoleHot, Paths: []string{"/hot"}, Media: []model.MediaType{model.Movie, model.TV}},
		{Name: "cold-tv", Role: model.RoleCold, Paths: []string{"/cold1"}, Media: []model.MediaType{model.TV}, TargetUsedPercent: 92, MaxUsedPercent: 95},
	}
}

func upcomingItem(title, rootFolderPath string, sizeBytes int64) ItemEval {
	return ItemEval{
		Item: model.MediaItem{
			ArrApp: "sonarr", ID: 1, Type: model.TV, Title: title,
			RootFolderPath: rootFolderPath, SizeBytes: sizeBytes, Upcoming: true,
		},
		Eval: scoring.Evaluation{Decision: scoring.Hot, Reasons: []string{"upcoming - not yet released/premiered"}},
	}
}

func cutoffUnmetItem(title, rootFolderPath string, sizeBytes int64) ItemEval {
	return ItemEval{
		Item: model.MediaItem{
			ArrApp: "sonarr", ID: 1, Type: model.TV, Title: title,
			RootFolderPath: rootFolderPath, SizeBytes: sizeBytes,
			Monitored: true, QualityCutoffNotMet: true,
		},
		Eval: scoring.Evaluation{Decision: scoring.Hot, Reasons: []string{"quality cutoff not met - expect this file to be replaced with an upgrade"}},
	}
}

func TestBuild_PromotesUpcomingItemOnColdBackToHot(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{upcomingItem("Show A", "/cold1", 5*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected 1 promotion move, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	e := plan.Entries[0]
	if e.FromTier != "cold-tv" || e.FromPath != "/cold1" || e.ToTier != "hot" || e.ToPath != "/hot" {
		t.Fatalf("unexpected move direction: %+v", e)
	}
}

func TestBuild_NeverReclaimsProtectedGrowRiskItems(t *testing.T) {
	tests := []struct {
		name string
		item ItemEval
	}{
		{name: "upcoming", item: upcomingItem("Protected upcoming show", "/cold1", 5*gib)},
		{name: "cutoff unmet", item: cutoffUnmetItem("Protected upgrade", "/cold1", 5*gib)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.item.Eval = scoring.Evaluation{Decision: scoring.Protected, Reasons: []string{"active download/import in progress"}}
			in := Input{
				Tiers: tvTiers(),
				Usage: map[string]diskusage.Usage{
					"/hot":   usage(1000*gib, 100*gib),
					"/cold1": usage(1000*gib, 100*gib),
				},
				Items:   []ItemEval{tt.item},
				History: emptyHistory(t),
				Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
				Now:     time.Now(),
			}

			plan, err := Build(in)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if len(plan.Entries) != 0 {
				t.Fatalf("protected item must never move despite grow-risk flags: %+v", plan.Entries)
			}
			if len(plan.Warnings) != 0 {
				t.Fatalf("protected item is not an unplaced candidate and should not warn: %v", plan.Warnings)
			}
		})
	}
}

func TestBuild_UpcomingOnColdWarnsWhenNoHotRoom(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(100*gib, 99*gib), // ~1 GB free - not enough
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{upcomingItem("Show A", "/cold1", 5*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected no move when hot has no room, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("expected a warning when no hot destination has room for a promotion")
	}
}

func TestBuild_UpcomingAlreadyOnHotIsNotPromoted(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{upcomingItem("Show A", "/hot", 5*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected no move for an upcoming item already on hot, got %d: %+v", len(plan.Entries), plan.Entries)
	}
}

func TestBuild_NonUpcomingOnColdIsNotPromoted(t *testing.T) {
	item := upcomingItem("Show A", "/cold1", 5*gib)
	item.Item.Upcoming = false // e.g. a continuing series scored Hot for some other reason
	item.Eval = scoring.Evaluation{Decision: scoring.Hot, Reasons: []string{"series is continuing/currently airing"}}

	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{item},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected promotion to stay scoped to Upcoming items only, got %d moves: %+v", len(plan.Entries), plan.Entries)
	}
}

func TestBuild_PromotionFreesColdRoomForSubsequentPacking(t *testing.T) {
	// A cold tier sitting right at its ceiling, holding one upcoming item
	// that needs to leave and one genuinely cold-eligible item on hot
	// that's waiting for room - the promotion should free enough space
	// for the hot->cold move to then succeed in the same pass.
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(100*gib, 95*gib), // at its 95% max already
		},
		Items: []ItemEval{
			upcomingItem("Show A (upcoming)", "/cold1", 10*gib),
			{
				Item: model.MediaItem{ArrApp: "sonarr", ID: 2, Type: model.TV, Title: "Show B", RootFolderPath: "/hot", SizeBytes: 5 * gib},
				Eval: scoring.Evaluation{Decision: scoring.Cold, Score: 80},
			},
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 2 {
		t.Fatalf("expected both the promotion and the freed-up hot->cold move, got %d: %+v", len(plan.Entries), plan.Entries)
	}
}

func TestBuild_PromotesQualityCutoffUnmetItemOnColdBackToHot(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{cutoffUnmetItem("Show A", "/cold1", 5*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected 1 reclaim move, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	e := plan.Entries[0]
	if e.FromTier != "cold-tv" || e.FromPath != "/cold1" || e.ToTier != "hot" || e.ToPath != "/hot" {
		t.Fatalf("unexpected move direction: %+v", e)
	}
	if e.FromRole != model.RoleCold {
		t.Fatalf("expected FromRole to be recorded as cold, got %q", e.FromRole)
	}
}

func TestBuild_QualityCutoffUnmetAlreadyOnHotIsNotPromoted(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{cutoffUnmetItem("Show A", "/hot", 5*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected no move for a cutoff-unmet item already on hot, got %d: %+v", len(plan.Entries), plan.Entries)
	}
}

func TestBuild_UnmonitoredCutoffUnmetOnColdIsNotPromoted(t *testing.T) {
	item := cutoffUnmetItem("Show A", "/cold1", 5*gib)
	item.Item.Monitored = false // Radarr/Sonarr won't actually search for an upgrade

	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{item},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected no move for an unmonitored cutoff-unmet item, got %d: %+v", len(plan.Entries), plan.Entries)
	}
}

func TestBuild_QualityCutoffPromotionFreesColdRoomForSubsequentPacking(t *testing.T) {
	// Same shape as TestBuild_PromotionFreesColdRoomForSubsequentPacking,
	// but for the quality-cutoff reclaim path: a cold tier at its ceiling,
	// holding one grow-risk item that needs to leave and one genuinely
	// cold-eligible item on hot waiting for room.
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(100*gib, 95*gib), // at its 95% max already
		},
		Items: []ItemEval{
			cutoffUnmetItem("Show A (grow-risk)", "/cold1", 10*gib),
			{
				Item: model.MediaItem{ArrApp: "sonarr", ID: 2, Type: model.TV, Title: "Show B", RootFolderPath: "/hot", SizeBytes: 5 * gib},
				Eval: scoring.Evaluation{Decision: scoring.Cold, Score: 80},
			},
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 2 {
		t.Fatalf("expected both the reclaim and the freed-up hot->cold move, got %d: %+v", len(plan.Entries), plan.Entries)
	}
}

// TestBuild_ReclaimSucceedsAfterFillFreesHotRoom proves the reverse
// dependency from TestBuild_QualityCutoffPromotionFreesColdRoomForSubsequentPacking:
// hot only has 7 GB free - not enough for the 10 GB reclaim - until an
// 8 GB fill (moving something else off hot to cold, which has plenty of
// room) frees up more, landing the reclaim's destination at 95% used,
// still under the hot tier's default 97% ceiling. The reclaim can
// only succeed in a later round than the fill, and its MoveEntry.Phase
// must reflect that so the mover executes the fill first for real, not
// just on paper.
func TestBuild_ReclaimSucceedsAfterFillFreesHotRoom(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(100*gib, 93*gib),   // 7 GB free
			"/cold1": usage(1000*gib, 100*gib), // plenty of room
		},
		Items: []ItemEval{
			cutoffUnmetItem("Show A (grow-risk)", "/cold1", 10*gib),
			{
				Item: model.MediaItem{ArrApp: "sonarr", ID: 2, Type: model.TV, Title: "Show B", RootFolderPath: "/hot", SizeBytes: 8 * gib},
				Eval: scoring.Evaluation{Decision: scoring.Cold, Score: 80},
			},
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("expected no warnings once both resolve, got %v", plan.Warnings)
	}
	if len(plan.Entries) != 2 {
		t.Fatalf("expected both the fill and the reclaim it enables, got %d: %+v", len(plan.Entries), plan.Entries)
	}

	var fillPhase, reclaimPhase int
	var foundFill, foundReclaim bool
	for _, e := range plan.Entries {
		switch e.Item.Title {
		case "Show B":
			foundFill = true
			fillPhase = e.Phase
			if e.FromTier != "hot" || e.ToTier != "cold-tv" {
				t.Errorf("fill entry has wrong direction: %+v", e)
			}
		case "Show A (grow-risk)":
			foundReclaim = true
			reclaimPhase = e.Phase
			if e.FromTier != "cold-tv" || e.ToTier != "hot" {
				t.Errorf("reclaim entry has wrong direction: %+v", e)
			}
		}
	}
	if !foundFill || !foundReclaim {
		t.Fatalf("expected both Show A and Show B in the plan, got %+v", plan.Entries)
	}
	if fillPhase >= reclaimPhase {
		t.Errorf("expected the fill's Phase (%d) strictly before the reclaim's Phase (%d) - the reclaim depends on the fill having already freed hot room", fillPhase, reclaimPhase)
	}
}

// TestBuild_ReclaimRespectsDefaultHotCeilingEvenWithRawBytesToSpare
// proves a reclaim is rejected once it would push its hot destination
// past the model's default ceiling, even though there is plainly enough raw
// free space for the bytes themselves - packing a hot tier to the wire
// defeats the reason reclaim exists (giving a grow-risk item room to
// actually grow) and risks real filesystem trouble besides.
func TestBuild_ReclaimRespectsDefaultHotCeilingEvenWithRawBytesToSpare(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			// 96 GB used of 100 GB (96% - under the 97% ceiling), with
			// enough raw room for a 2 GB reclaim. It would land at 98%, so
			// only the percentage ceiling rejects it.
			"/hot":   usage(100*gib, 96*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{cutoffUnmetItem("Show A (grow-risk)", "/cold1", 2*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected the reclaim to be rejected by the default hot ceiling, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("expected a warning when the reclaim would cross the default hot ceiling")
	}
}

// TestBuild_ExplicitHotMaxUsedPercentOverridesDefault confirms a hot
// tier that sets its own MaxUsedPercent uses that instead of the model
// default - an operator who deliberately wants a hot
// tier packed tighter (or looser) than the built-in default can do so.
func TestBuild_ExplicitHotMaxUsedPercentOverridesDefault(t *testing.T) {
	tiers := tvTiers()
	for i := range tiers {
		if tiers[i].Role == model.RoleHot {
			tiers[i].MaxUsedPercent = 99
		}
	}

	in := Input{
		Tiers: tiers,
		Usage: map[string]diskusage.Usage{
			// 88 GB used + 10 GB reclaim lands at 98% used: over the
			// built-in default ceiling (97, would reject - see
			// TestBuild_ReclaimRespectsDefaultHotCeilingEvenWithRawBytesToSpare)
			// but under this tier's explicit override (99, should accept).
			"/hot":   usage(100*gib, 88*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{cutoffUnmetItem("Show A (grow-risk)", "/cold1", 10*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected the reclaim to succeed under the tier's own 99%% MaxUsedPercent, got %d entries and warnings %v", len(plan.Entries), plan.Warnings)
	}
}

func TestBuild_ReclaimAllocationPlacesLargerConstrainedItemFirst(t *testing.T) {
	tiers := []model.Tier{
		{Name: "hot-large", Role: model.RoleHot, Paths: []string{"/hot-large"}, Media: []model.MediaType{model.TV}, MaxUsedPercent: 100},
		{Name: "hot-small", Role: model.RoleHot, Paths: []string{"/hot-small"}, Media: []model.MediaType{model.TV}, MaxUsedPercent: 100},
		{Name: "cold", Role: model.RoleCold, Paths: []string{"/cold"}, Media: []model.MediaType{model.TV}, TargetUsedPercent: 92, MaxUsedPercent: 95},
	}
	small := upcomingItem("Small reclaim", "/cold", 15*gib)
	large := upcomingItem("Large reclaim", "/cold", 25*gib)
	large.Item.ID = 2
	in := Input{
		Tiers: tiers,
		Usage: map[string]diskusage.Usage{
			"/hot-large": usage(100*gib, 73*gib), // 27 GB writable
			"/hot-small": usage(100*gib, 83*gib), // 17 GB writable
			"/cold":      usage(1000*gib, 100*gib),
		},
		// Inventory order is deliberately the bad greedy order.
		Items:   []ItemEval{small, large},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 2 || len(plan.Warnings) != 0 {
		t.Fatalf("both reclaims have a feasible allocation, got entries=%+v warnings=%v", plan.Entries, plan.Warnings)
	}
	destination := map[string]string{}
	for _, entry := range plan.Entries {
		destination[entry.Item.Title] = entry.ToPath
	}
	if destination["Large reclaim"] != "/hot-large" || destination["Small reclaim"] != "/hot-small" {
		t.Fatalf("expected 25 GB on the only large slot and 15 GB on the small slot, got %v", destination)
	}
}

func TestBuild_ReclaimAllocationPreservesMediaSpecificDestination(t *testing.T) {
	tiers := []model.Tier{
		{Name: "hot-shared", Role: model.RoleHot, Paths: []string{"/hot-shared"}, Media: []model.MediaType{model.Movie, model.TV}, MaxUsedPercent: 100},
		{Name: "hot-tv", Role: model.RoleHot, Paths: []string{"/hot-tv"}, Media: []model.MediaType{model.TV}, MaxUsedPercent: 100},
		{Name: "cold", Role: model.RoleCold, Paths: []string{"/cold"}, Media: []model.MediaType{model.Movie, model.TV}, TargetUsedPercent: 92, MaxUsedPercent: 95},
	}
	tv := upcomingItem("TV reclaim", "/cold", 10*gib)
	movie := upcomingItem("Movie reclaim", "/cold", 10*gib)
	movie.Item.ArrApp = "radarr"
	movie.Item.ID = 2
	movie.Item.Type = model.Movie
	in := Input{
		Tiers: tiers,
		Usage: map[string]diskusage.Usage{
			"/hot-shared": usage(100*gib, 90*gib),
			"/hot-tv":     usage(100*gib, 90*gib),
			"/cold":       usage(1000*gib, 100*gib),
		},
		// TV is first to reproduce the order that used to consume the only
		// movie-capable slot and strand the movie.
		Items:   []ItemEval{tv, movie},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 2 || len(plan.Warnings) != 0 {
		t.Fatalf("both media-specific reclaims have a feasible allocation, got entries=%+v warnings=%v", plan.Entries, plan.Warnings)
	}
	destination := map[string]string{}
	for _, entry := range plan.Entries {
		destination[entry.Item.Title] = entry.ToPath
	}
	if destination["Movie reclaim"] != "/hot-shared" || destination["TV reclaim"] != "/hot-tv" {
		t.Fatalf("expected movie to retain the shared slot and TV to use its dedicated slot, got %v", destination)
	}
}

func TestBuild_ReclaimAllocationRepairsGreedyBinPacking(t *testing.T) {
	tiers := []model.Tier{
		{Name: "hot-five", Role: model.RoleHot, Paths: []string{"/hot-five"}, Media: []model.MediaType{model.TV}, MaxUsedPercent: 100},
		{Name: "hot-six", Role: model.RoleHot, Paths: []string{"/hot-six"}, Media: []model.MediaType{model.TV}, MaxUsedPercent: 100},
		{Name: "cold", Role: model.RoleCold, Paths: []string{"/cold"}, Media: []model.MediaType{model.TV}, TargetUsedPercent: 92, MaxUsedPercent: 95},
	}
	sizes := []int64{2 * gib, 2 * gib, 3 * gib, 4 * gib}
	items := make([]ItemEval, 0, len(sizes))
	for i, sizeBytes := range sizes {
		item := upcomingItem(fmt.Sprintf("Reclaim %d", i+1), "/cold", sizeBytes)
		item.Item.ID = i + 1
		items = append(items, item)
	}
	in := Input{
		Tiers: tiers,
		Usage: map[string]diskusage.Usage{
			"/hot-five": usage(100*gib, 95*gib),
			"/hot-six":  usage(100*gib, 94*gib),
			"/cold":     usage(1000*gib, 100*gib),
		},
		Items:   items,
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 4 || len(plan.Warnings) != 0 {
		t.Fatalf("all four reclaims fit as 3+2 and 4+2, got entries=%+v warnings=%v", plan.Entries, plan.Warnings)
	}
	placedBytes := map[string]int64{}
	for _, entry := range plan.Entries {
		placedBytes[entry.ToPath] += entry.Item.SizeBytes
	}
	if placedBytes["/hot-five"] != 5*gib || placedBytes["/hot-six"] != 6*gib {
		t.Fatalf("expected exact 5/6 GB partition, got %v", placedBytes)
	}
}

func TestBuild_FirstEnabledReclaimDoesNotWaitForLargerPeer(t *testing.T) {
	tiers := []model.Tier{
		{Name: "hot", Role: model.RoleHot, Paths: []string{"/hot"}, Media: []model.MediaType{model.TV}, MaxUsedPercent: 100},
		{Name: "cold", Role: model.RoleCold, Paths: []string{"/cold"}, Media: []model.MediaType{model.TV}, TargetUsedPercent: 100, MaxUsedPercent: 100},
	}
	small := upcomingItem("Small reclaim", "/cold", 2*gib)
	large := upcomingItem("Large reclaim", "/cold", 8*gib)
	large.Item.ID = 2
	items := []ItemEval{small, large}
	for i := 0; i < 5; i++ {
		items = append(items, ItemEval{
			Item: model.MediaItem{ArrApp: "sonarr", ID: 100 + i, Type: model.TV, Title: fmt.Sprintf("Fill %d", i+1), RootFolderPath: "/hot", SizeBytes: 2 * gib},
			Eval: scoring.Evaluation{Decision: scoring.Cold, Score: float64(100 - i)},
		})
	}
	in := Input{
		Tiers: tiers,
		Usage: map[string]diskusage.Usage{
			"/hot":  usage(100*gib, 100*gib),
			"/cold": usage(100*gib, 50*gib),
		},
		Items:   items,
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 7 || len(plan.Warnings) != 0 {
		t.Fatalf("expected all fills and reclaims, got entries=%+v warnings=%v", plan.Entries, plan.Warnings)
	}
	phaseByTitle := map[string]int{}
	for _, entry := range plan.Entries {
		phaseByTitle[entry.Item.Title] = entry.Phase
	}
	smallPhase := phaseByTitle["Small reclaim"]
	largePhase := phaseByTitle["Large reclaim"]
	fillsBeforeSmall := 0
	for i := 0; i < 5; i++ {
		if phaseByTitle[fmt.Sprintf("Fill %d", i+1)] < smallPhase {
			fillsBeforeSmall++
		}
	}
	if fillsBeforeSmall != 1 {
		t.Fatalf("small reclaim should run immediately after its one enabling fill, got %d fills first: %+v", fillsBeforeSmall, plan.Entries)
	}
	if largePhase <= smallPhase {
		t.Fatalf("8 GB reclaim should wait for the later four-fill wave, phases small=%d large=%d", smallPhase, largePhase)
	}
}

// TestBuild_ReclaimsRunAsSoonAsFillsMakeEnoughRoom guards against bunching
// all hot-bound work at the end. Hot starts past its percentage ceiling, so
// three fills genuinely must precede the two reclaims; the other three fills
// are unrelated and must remain behind the now-enabled reclaim phase.
func TestBuild_ReclaimsRunAsSoonAsFillsMakeEnoughRoom(t *testing.T) {
	items := []ItemEval{
		// Two grow-risk items on cold: 10 GB each, and hot has ~13 GB
		// free, so the bytes were never the problem.
		cutoffUnmetItem("Show A (grow-risk)", "/cold1", 10*gib),
		upcomingItem("Show B (upcoming)", "/cold1", 10*gib),
	}
	items[1].Item.ID = 99 // upcomingItem and cutoffUnmetItem both default to ID 1
	for i := 2; i < 8; i++ {
		items = append(items, ItemEval{
			Item: model.MediaItem{ArrApp: "sonarr", ID: 100 + i, Type: model.TV, Title: fmt.Sprintf("Fill %d", i), RootFolderPath: "/hot", SizeBytes: 12 * gib},
			Eval: scoring.Evaluation{Decision: scoring.Cold, Score: float64(60 + i)},
		})
	}

	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 986*gib), // 98.6% used, but 14 GB free
			"/cold1": usage(10000*gib, 1000*gib),
		},
		Items:   items,
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	var fillsBefore, fillsAfter, reclaims int
	reclaimPhase := -1
	for _, e := range plan.Entries {
		if e.ToPath == "/hot" {
			reclaims++
			if reclaimPhase < 0 {
				reclaimPhase = e.Phase
			} else if e.Phase != reclaimPhase {
				t.Errorf("expected both reclaims in one enabled batch, got phases %d and %d", reclaimPhase, e.Phase)
			}
			continue
		}
		if reclaimPhase < 0 {
			fillsBefore++
		} else {
			fillsAfter++
		}
	}
	if reclaims != 2 {
		t.Fatalf("expected both grow-risk items reclaimed once fills made room, got %d: %+v", reclaims, plan.Entries)
	}
	if fillsBefore != 3 || fillsAfter != 3 {
		t.Fatalf("expected exactly 3 enabling fills, then reclaims, then 3 unrelated fills; got %d before / %d after: %+v", fillsBefore, fillsAfter, plan.Entries)
	}
	if reclaimPhase <= 0 {
		t.Errorf("reclaims ran in phase %d even though hot needed an earlier fill phase", reclaimPhase)
	}
	if final := plan.FinalUsage["/hot"]; final.UsedPercent > model.DefaultHotMaxUsedPercent {
		t.Errorf("hot ended above its ceiling: %.2f%%", final.UsedPercent)
	}
}

// TestBuild_StillWarnsWhenNoRoundEverMakesRoom confirms a reclaim that
// can never fit - hot has zero free room and nothing exists to ever
// free any - ends up as a warning and the loop terminates, rather than
// spinning or silently vanishing.
func TestBuild_StillWarnsWhenNoRoundEverMakesRoom(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(100*gib, 100*gib), // completely full
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{cutoffUnmetItem("Show A", "/cold1", 10*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected no moves when hot never has room, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("expected a warning when the reclaim never fits in any round")
	}
}

// favoriteItem is a Jellyfin-favorited series sitting at rootFolderPath.
// A Favorite is an explicit user statement that this title belongs on fast
// storage, so scoring hands the planner Hot (see scoring.Evaluate) - it is
// only Protected when something stricter (never-move tag, active import)
// also applies, which the tests below cover separately.
func favoriteItem(title, rootFolderPath string, sizeBytes int64) ItemEval {
	return ItemEval{
		Item: model.MediaItem{
			ArrApp: "sonarr", ID: 1, Type: model.TV, Title: title,
			RootFolderPath: rootFolderPath, SizeBytes: sizeBytes,
			Monitored: true, HasFile: true, JellyfinFavorite: true,
		},
		Eval: scoring.Evaluation{Decision: scoring.Hot, Reasons: []string{"marked Favorite in Jellyfin"}},
	}
}

func TestBuild_ReclaimsFavoriteOnColdBackToHot(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{favoriteItem("Bonkers", "/cold1", 5*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected the favorite to be reclaimed to hot, got %d moves: %+v", len(plan.Entries), plan.Entries)
	}
	e := plan.Entries[0]
	if e.FromTier != "cold-tv" || e.FromPath != "/cold1" || e.ToTier != "hot" || e.ToPath != "/hot" {
		t.Fatalf("unexpected move direction: %+v", e)
	}
}

// TestBuild_FavoriteOnColdEvictsColdCandidateToMakeRoom is the issue #77
// reproduction: Coldarr moved a show to cold, the user then favorited it in
// Jellyfin, and the next plan must pull it back - evicting an ordinary
// cold-eligible item from hot first when hot has no room of its own. The
// favorite is deliberately inside the move cooldown, because "Coldarr just
// moved this and I immediately favorited it" is the literal bug report.
func TestBuild_FavoriteOnColdEvictsColdCandidateToMakeRoom(t *testing.T) {
	favorite := favoriteItem("Bonkers", "/cold1", 10*gib)

	h := emptyHistory(t)
	if err := h.Append(history.Record{
		ArrApp:  favorite.Item.ArrApp,
		ItemID:  favorite.Item.ID,
		MovedAt: time.Now().Add(-time.Hour), // Coldarr moved it to cold an hour ago
	}); err != nil {
		t.Fatalf("history.Append: %v", err)
	}

	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(100*gib, 93*gib),   // 7 GB free - not enough for a 10 GB reclaim
			"/cold1": usage(1000*gib, 100*gib), // plenty of room to take the evicted item
		},
		Items: []ItemEval{
			favorite,
			{
				Item: model.MediaItem{ArrApp: "sonarr", ID: 2, Type: model.TV, Title: "Show B", RootFolderPath: "/hot", SizeBytes: 8 * gib},
				Eval: scoring.Evaluation{Decision: scoring.Cold, Score: 80},
			},
		},
		History: h,
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("expected no warnings once the eviction makes room, got %v", plan.Warnings)
	}
	if len(plan.Entries) != 2 {
		t.Fatalf("expected the eviction and the favorite reclaim it enables, got %d: %+v", len(plan.Entries), plan.Entries)
	}

	var fillPhase, reclaimPhase int
	var foundFill, foundReclaim bool
	for _, e := range plan.Entries {
		switch e.Item.Title {
		case "Show B":
			foundFill = true
			fillPhase = e.Phase
			if e.FromTier != "hot" || e.ToTier != "cold-tv" {
				t.Errorf("eviction entry has wrong direction: %+v", e)
			}
		case "Bonkers":
			foundReclaim = true
			reclaimPhase = e.Phase
			if e.FromTier != "cold-tv" || e.ToTier != "hot" {
				t.Errorf("favorite reclaim entry has wrong direction: %+v", e)
			}
		}
	}
	if !foundFill || !foundReclaim {
		t.Fatalf("expected both Bonkers and Show B in the plan, got %+v", plan.Entries)
	}
	if fillPhase >= reclaimPhase {
		t.Errorf("expected the eviction's Phase (%d) strictly before the favorite reclaim's Phase (%d) - the reclaim depends on the eviction having already freed hot room", fillPhase, reclaimPhase)
	}
}

// TestBuild_FavoriteReclaimIgnoresCooldownAndMinMoveSize pins the two gates
// that apply to ordinary hot->cold packing but deliberately do not apply to
// reclaims. Favoriting a title is an explicit request that must take effect
// on the next plan, however recently Coldarr moved it and however small it
// is.
func TestBuild_FavoriteReclaimIgnoresCooldownAndMinMoveSize(t *testing.T) {
	favorite := favoriteItem("Bonkers", "/cold1", 2*gib)

	h := emptyHistory(t)
	if err := h.Append(history.Record{
		ArrApp:  favorite.Item.ArrApp,
		ItemID:  favorite.Item.ID,
		MovedAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("history.Append: %v", err)
	}

	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{favorite},
		History: h,
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 50}, // both gates would block a fill
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("expected the favorite reclaim to bypass cooldown and min move size, got %d moves: %+v", len(plan.Entries), plan.Entries)
	}
	if e := plan.Entries[0]; e.ToTier != "hot" {
		t.Fatalf("expected a cold->hot reclaim, got %+v", e)
	}
}

// TestBuild_FavoriteAlreadyOnHotIsLeftAlone is the other half of the
// guarantee: calling a Favorite Hot rather than Protected must not make it
// eligible for hot->cold packing, even when everything else about it says
// "cold" and cold has room waiting.
func TestBuild_FavoriteAlreadyOnHotIsLeftAlone(t *testing.T) {
	favorite := favoriteItem("Bonkers", "/hot", 50*gib)
	favorite.Item.Tags = []string{"cold-ok"}
	favorite.Item.Added = time.Now().AddDate(-5, 0, 0)

	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(100*gib, 95*gib), // hot is nearly full - pressure to evict
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{favorite},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("a favorite on hot must never be moved to cold, got %+v", plan.Entries)
	}
}

func TestBuild_FavoriteOnColdWarnsWhenNoHotRoom(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(100*gib, 99*gib), // ~1 GB free - not enough
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{favoriteItem("Bonkers", "/cold1", 5*gib)},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected no move when hot has no room, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("expected a warning when no hot destination has room for a favorite reclaim")
	}
}

// TestBuild_NeverReclaimsFavoriteThatIsAlsoProtected is the safety edge of
// #77: a Favorite that is simultaneously mid-import or tagged never-move is
// still Protected by scoring, and Protected stays absolute - the reclaim
// path must not reach around it.
func TestBuild_NeverReclaimsFavoriteThatIsAlsoProtected(t *testing.T) {
	tests := []struct {
		name   string
		reason string
	}{
		{name: "active import", reason: "active download/import in progress"},
		{name: "never-move tag", reason: "tagged never-move"},
		{name: "protected tag", reason: "tagged protected/keep-hot"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := favoriteItem("Bonkers", "/cold1", 5*gib)
			item.Eval = scoring.Evaluation{Decision: scoring.Protected, Reasons: []string{tt.reason}}

			in := Input{
				Tiers: tvTiers(),
				Usage: map[string]diskusage.Usage{
					"/hot":   usage(1000*gib, 100*gib), // room to spare, so only the Protected check can stop it
					"/cold1": usage(1000*gib, 100*gib),
				},
				Items:   []ItemEval{item},
				History: emptyHistory(t),
				Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
				Now:     time.Now(),
			}

			plan, err := Build(in)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if len(plan.Entries) != 0 {
				t.Fatalf("a protected favorite must never move: %+v", plan.Entries)
			}
			if len(plan.Warnings) != 0 {
				t.Fatalf("a protected favorite is not an unplaced candidate and should not warn: %v", plan.Warnings)
			}
		})
	}
}

// TestBuild_NonFavoriteHotItemOnColdIsNotReclaimed keeps the new reclaim
// case scoped: being Hot for some ordinary reason (inside the grace period,
// a continuing series, a below-threshold score) is not on its own grounds to
// pull a title back off cold storage.
func TestBuild_NonFavoriteHotItemOnColdIsNotReclaimed(t *testing.T) {
	item := favoriteItem("Show A", "/cold1", 5*gib)
	item.Item.JellyfinFavorite = false
	item.Eval = scoring.Evaluation{Decision: scoring.Hot, Reasons: []string{"series is continuing/currently airing"}}

	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 100*gib),
			"/cold1": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{item},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(plan.Entries) != 0 {
		t.Fatalf("expected reclaim to stay scoped to favorites/upcoming/cutoff-unmet, got %d moves: %+v", len(plan.Entries), plan.Entries)
	}
}

// TestBuild_FavoriteScoredByRealPolicyIsReclaimed wires the real scoring
// policy into Build instead of a hand-written Evaluation, so the two halves
// of the #77 fix cannot drift apart: whatever Decision scoring.Evaluate
// hands a favorite has to be one misplacedOnCold is willing to act on. The
// identical item without the star is the control - it is genuinely cold by
// every other measure and must be left on cold storage.
func TestBuild_FavoriteScoredByRealPolicyIsReclaimed(t *testing.T) {
	policy := config.PolicyConfig{
		CooldownDays:            30,
		HotGraceDays:            14,
		MinMoveSizeGB:           1,
		NeverMoveTags:           []string{"never-move"},
		ProtectedTags:           []string{"keep-hot"},
		ColdOkTags:              []string{"cold-ok"},
		ProtectContinuingSeries: true,
		ColdScoreThreshold:      40,
	}
	now := time.Now()
	lastAired := now.AddDate(-2, 0, 0)

	for _, tt := range []struct {
		name          string
		favorite      bool
		wantReclaimed bool
	}{
		{name: "favorited", favorite: true, wantReclaimed: true},
		{name: "not favorited", favorite: false, wantReclaimed: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			item := model.MediaItem{
				ArrApp: "sonarr", ID: 1, Type: model.TV, Title: "Bonkers",
				RootFolderPath: "/cold1", SizeBytes: 10 * gib,
				Added: now.AddDate(-3, 0, 0), Ended: true, LastAired: &lastAired,
				Monitored: true, HasFile: true,
				JellyfinFavorite: tt.favorite,
			}

			eval := scoring.Evaluate(item, policy, now)
			if tt.favorite && eval.Decision != scoring.Hot {
				t.Fatalf("precondition: expected a favorite to score Hot, got %v", eval.Decision)
			}
			if !tt.favorite && eval.Decision != scoring.Cold {
				t.Fatalf("precondition: expected the unfavorited control to score Cold, got %v (score %.1f)", eval.Decision, eval.Score)
			}

			in := Input{
				Tiers: tvTiers(),
				Usage: map[string]diskusage.Usage{
					"/hot":   usage(1000*gib, 100*gib),
					"/cold1": usage(1000*gib, 100*gib),
				},
				Items:   []ItemEval{{Item: item, Eval: eval}},
				History: emptyHistory(t),
				Policy:  policy,
				Now:     now,
			}

			plan, err := Build(in)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}

			reclaimed := len(plan.Entries) == 1 && plan.Entries[0].ToTier == "hot"
			if reclaimed != tt.wantReclaimed {
				t.Fatalf("reclaimed = %v, want %v (entries: %+v)", reclaimed, tt.wantReclaimed, plan.Entries)
			}
		})
	}
}
