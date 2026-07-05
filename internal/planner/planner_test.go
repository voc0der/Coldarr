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
