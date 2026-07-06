package webui

import (
	"testing"
	"time"
)

// TestCurrentApplyStatus_HidesResultPastTTL covers the real complaint this
// exists for: a finished apply's result card must not sit pinned to the
// top of the Plan page forever - once it's old news, the page should look
// exactly like one that's never had an apply run, so a fresh "Nothing to
// move" (or a new plan) doesn't read as "stuck" behind a stale banner.
func TestCurrentApplyStatus_HidesResultPastTTL(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)

	eng, err := srv.newEngine()
	if err != nil {
		t.Fatalf("newEngine: %v", err)
	}
	now := time.Now()
	inv, err := eng.BuildInventory(now)
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	plan, err := eng.BuildPlan(inv, now)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Entries) == 0 {
		t.Fatal("test setup: expected at least one planned move")
	}

	progress, err := srv.startApply(eng, inv, plan)
	if err != nil {
		t.Fatalf("startApply: %v", err)
	}
	progress.Wait()

	if status := srv.currentApplyStatus(); status.NoRun || status.Finished == "" {
		t.Fatalf("expected a freshly-finished result to be shown with a timestamp, got %+v", status)
	}

	old := finishedResultTTL
	finishedResultTTL = time.Millisecond
	t.Cleanup(func() { finishedResultTTL = old })
	time.Sleep(5 * time.Millisecond)

	if status := srv.currentApplyStatus(); !status.NoRun {
		t.Fatalf("expected result past its TTL to be hidden entirely, got %+v", status)
	}
}
