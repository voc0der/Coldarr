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
			Name:              "hot",
			Role:              model.RoleHot,
			Paths:             []string{"/hot"},
			Media:             []model.MediaType{model.Movie, model.TV},
			TargetUsedPercent: 75,
			MaxUsedPercent:    80,
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

func TestBuild_NoMoveWhenUnderThreshold(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 700*gib), // 70%, under max of 80%
			"/cold1": usage(1000*gib, 100*gib),
			"/cold2": usage(1000*gib, 100*gib),
		},
		Items:   []ItemEval{coldItem(1, "Movie A", 50*gib)},
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

func TestBuild_MovesColdItemsWhenHotOverPressure(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 850*gib), // 85%, over max of 80%, target 75%
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
	// Need to free 10% of 1000GB = 100GB. Both items should move
	// (highest score first) since one alone isn't enough.
	if len(plan.Entries) != 2 {
		t.Fatalf("expected 2 moves, got %d: %+v", len(plan.Entries), plan.Entries)
	}
	// Item 2 has the higher score (52 vs 51) so it should be picked first.
	if plan.Entries[0].Item.Title != "Movie B" {
		t.Fatalf("expected coldest item first, got %q", plan.Entries[0].Item.Title)
	}
	for _, e := range plan.Entries {
		if e.FromPath != "/hot" || e.ToTier != "cold-movies" {
			t.Fatalf("unexpected move: %+v", e)
		}
	}
}

func TestBuild_NeverExceedsDestinationCeiling(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot":   usage(1000*gib, 900*gib), // way over pressure
			"/cold1": usage(100*gib, 94*gib),   // almost at its 95% ceiling
			"/cold2": usage(100*gib, 10*gib),   // plenty of room
		},
		Items: []ItemEval{
			coldItem(1, "Big Movie", 50*gib), // would push cold1 past 95% if placed there
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
		t.Fatalf("expected 1 move, got %d", len(plan.Entries))
	}
	if plan.Entries[0].ToPath != "/cold2" {
		t.Fatalf("expected item routed to /cold2 (only path with room), got %s", plan.Entries[0].ToPath)
	}
	final := plan.FinalUsage["/cold2"]
	if final.UsedPercent > 95 {
		t.Fatalf("destination exceeded its ceiling: %.1f%%", final.UsedPercent)
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
			"/hot":   usage(1000*gib, 850*gib),
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
	if len(plan.Warnings) == 0 {
		t.Fatalf("expected a warning that target could not be reached")
	}
}

func TestBuild_UnavailablePathIsSkippedNotAssumedEmpty(t *testing.T) {
	in := Input{
		Tiers: testTiers(),
		Usage: map[string]diskusage.Usage{
			"/hot": usage(1000*gib, 850*gib),
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
