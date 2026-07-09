package planner

import (
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/scoring"
)

const gib = 1 << 30

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
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot": usage(1000*gib, 500*gib),
			// Raw total says 10 GB "free" (100-90), but only 5 GB of that
			// is actually writable (Bavail) - 5 GB is reserved and
			// invisible to df's Use% and to any unprivileged writer.
			"/cold1": {TotalBytes: 100 * gib, UsedBytes: 90 * gib, FreeBytes: 5 * gib, UsedPercent: 90.0 / 95.0 * 100},
			"/cold2": usage(1000*gib, 100*gib),
		},
		Items: []ItemEval{
			// Bigger than the real 5 GB free, but old total-based math
			// (90+6)/100=96% would have looked fine under a 97% ceiling.
			coldItem(1, "Movie A", 6*gib),
		},
		History: emptyHistory(t),
		Policy:  config.PolicyConfig{CooldownDays: 30, MinMoveSizeGB: 1},
		Now:     time.Now(),
	}
	in.Tiers[1].MaxUsedPercent = 97
	in.Tiers[1].TargetUsedPercent = 97

	plan, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, e := range plan.Entries {
		if e.ToPath == "/cold1" {
			t.Fatalf("expected /cold1 to be rejected (only 5 GB actually free for a 6 GB item), got it picked: %+v", e)
		}
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
// still under pickHotDestination's default 97% ceiling. The reclaim can
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
// past defaultHotMaxUsedPercent, even though there is plainly enough raw
// free space for the bytes themselves - packing a hot tier to the wire
// defeats the reason reclaim exists (giving a grow-risk item room to
// actually grow) and risks real filesystem trouble besides.
func TestBuild_ReclaimRespectsDefaultHotCeilingEvenWithRawBytesToSpare(t *testing.T) {
	in := Input{
		Tiers: tvTiers(),
		Usage: map[string]diskusage.Usage{
			// 96 GB used of 100 GB (96% - under the 97% ceiling), but a
			// 10 GB reclaim would land at 106% used - well past both the
			// ceiling and raw capacity.
			"/hot":   usage(100*gib, 96*gib),
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
	if len(plan.Entries) != 0 {
		t.Fatalf("expected the reclaim to be rejected by the default hot ceiling, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("expected a warning when the reclaim would cross the default hot ceiling")
	}
}

// TestBuild_ExplicitHotMaxUsedPercentOverridesDefault confirms a hot
// tier that sets its own MaxUsedPercent uses that instead of
// defaultHotMaxUsedPercent - an operator who deliberately wants a hot
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
