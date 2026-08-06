package report

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/engine"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/planner"
	"github.com/vocoder/coldarr/internal/scoring"
)

func testInventory() *engine.Inventory {
	hotTier := model.Tier{Name: "hot", Role: model.RoleHot, Paths: []string{"/hot"}}
	coldTier := model.Tier{Name: "cold", Role: model.RoleCold, Paths: []string{"/cold1", "/cold2"}, TargetUsedPercent: 90, MaxUsedPercent: 95}

	inv := &engine.Inventory{
		Tiers: []model.Tier{hotTier, coldTier},
		PathStatus: map[string]engine.PathStatus{
			"/hot":   {Tier: hotTier, Path: "/hot", Usage: diskusage.Usage{TotalBytes: 100, UsedBytes: 50, FreeBytes: 50, UsedPercent: 50}, DeviceID: 1, DeviceIDKnown: true},
			"/cold1": {Tier: coldTier, Path: "/cold1", Usage: diskusage.Usage{TotalBytes: 100, UsedBytes: 80, FreeBytes: 20, UsedPercent: 80}, DeviceID: 2, DeviceIDKnown: true},
			"/cold2": {Tier: coldTier, Path: "/cold2", Err: fmt.Errorf("path does not exist")},
		},
		Items: []planner.ItemEval{
			{Item: model.MediaItem{Title: "Cold Candidate", RootFolderPath: "/hot"}, Eval: scoring.Evaluation{Decision: scoring.Cold, Score: 42, Reasons: []string{"old and unwatched"}}},
			{Item: model.MediaItem{Title: "Protected Item", RootFolderPath: "/hot"}, Eval: scoring.Evaluation{Decision: scoring.Protected}},
			{Item: model.MediaItem{Title: "Misplaced Item", RootFolderPath: "/cold1"}, Eval: scoring.Evaluation{Decision: scoring.Hot, Reasons: []string{"recently added"}}},
		},
	}
	return inv
}

func TestTierUsage(t *testing.T) {
	var buf bytes.Buffer
	TierUsage(&buf, testInventory())
	out := buf.String()

	if !strings.Contains(out, "hot") || !strings.Contains(out, "cold") {
		t.Errorf("expected both tier names in output:\n%s", out)
	}
	if !strings.Contains(out, "UNAVAILABLE") {
		t.Errorf("expected the errored /cold2 path to be reported as UNAVAILABLE:\n%s", out)
	}
	if !strings.Contains(out, "97.0 (default)") {
		t.Errorf("expected the hot tier's effective reclaim ceiling to be visible:\n%s", out)
	}
}

func TestTierUsage_PreservesFractionalThresholds(t *testing.T) {
	inv := testInventory()
	inv.Tiers[0].MaxUsedPercent = 96.5
	inv.Tiers[1].TargetUsedPercent = 90.5
	inv.Tiers[1].MaxUsedPercent = 95.5

	var buf bytes.Buffer
	TierUsage(&buf, inv)
	out := buf.String()
	for _, want := range []string{"96.5 (configured)", "90.5", "95.5"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected fractional threshold %q to be preserved:\n%s", want, out)
		}
	}
}

func TestTierUsage_SharedVolumeNote(t *testing.T) {
	inv := testInventory()
	// Make /cold1 share a device with /hot, to exercise the "shares a
	// disk with" annotation.
	status := inv.PathStatus["/cold1"]
	status.DeviceID = inv.PathStatus["/hot"].DeviceID
	inv.PathStatus["/cold1"] = status

	var buf bytes.Buffer
	TierUsage(&buf, inv)
	if !strings.Contains(buf.String(), "shares a disk with") {
		t.Errorf("expected a shared-disk annotation:\n%s", buf.String())
	}
}

func TestWarnings(t *testing.T) {
	var buf bytes.Buffer
	Warnings(&buf, []string{"quality-cutoff status has not been scanned"})
	if !strings.Contains(buf.String(), "quality-cutoff status has not been scanned") {
		t.Errorf("expected the warning text in output:\n%s", buf.String())
	}

	buf.Reset()
	Warnings(&buf, nil)
	if buf.Len() != 0 {
		t.Errorf("expected no output for zero warnings, got:\n%s", buf.String())
	}
}

func TestSummary(t *testing.T) {
	var buf bytes.Buffer
	Summary(&buf, testInventory(), 5)
	out := buf.String()

	if !strings.Contains(out, "3 items") || !strings.Contains(out, "1 protected") || !strings.Contains(out, "1 hot") || !strings.Contains(out, "1 cold") {
		t.Errorf("expected decision counts in the inventory summary line:\n%s", out)
	}
	if !strings.Contains(out, "Cold Candidate") {
		t.Errorf("expected the cold-on-hot candidate to be listed:\n%s", out)
	}
	if !strings.Contains(out, "Misplaced Item") {
		t.Errorf("expected the hot-on-cold misplaced item to be listed:\n%s", out)
	}
}

func TestPlan_NoMoves(t *testing.T) {
	var buf bytes.Buffer
	Plan(&buf, &planner.Plan{})
	if !strings.Contains(buf.String(), "No moves needed") {
		t.Errorf("expected a no-moves message, got:\n%s", buf.String())
	}
}

func TestPlan_WithEntries(t *testing.T) {
	p := &planner.Plan{
		Entries: []planner.MoveEntry{
			{Item: model.MediaItem{Title: "Movie A", SizeBytes: 5 << 30}, FromTier: "hot", FromPath: "/hot", ToTier: "cold", ToPath: "/cold1", Score: 10, Reasons: []string{"old"}},
		},
		Warnings: []string{"cold2 is unavailable"},
	}
	var buf bytes.Buffer
	Plan(&buf, p)
	out := buf.String()

	if !strings.Contains(out, "Movie A") || !strings.Contains(out, "/hot") || !strings.Contains(out, "/cold1") {
		t.Errorf("expected the move entry's details in output:\n%s", out)
	}
	if !strings.Contains(out, "1 items") {
		t.Errorf("expected an item count summary:\n%s", out)
	}
	if !strings.Contains(out, "cold2 is unavailable") {
		t.Errorf("expected plan warnings in output:\n%s", out)
	}
}

func TestProjectedUsage_OnlyShowsChangedPaths(t *testing.T) {
	before := map[string]diskusage.Usage{
		"/cold1": {UsedPercent: 80},
		"/cold2": {UsedPercent: 50},
	}
	after := map[string]diskusage.Usage{
		"/cold1": {UsedPercent: 85},
		"/cold2": {UsedPercent: 50}, // unchanged
	}
	tiers := []model.Tier{{Name: "cold", Paths: []string{"/cold1", "/cold2"}}}

	var buf bytes.Buffer
	ProjectedUsage(&buf, before, after, tiers)
	out := buf.String()

	if !strings.Contains(out, "/cold1") {
		t.Errorf("expected the changed path /cold1 in output:\n%s", out)
	}
	if strings.Contains(out, "/cold2") {
		t.Errorf("expected the unchanged path /cold2 to be omitted:\n%s", out)
	}
}
