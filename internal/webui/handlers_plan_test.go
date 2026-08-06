package webui

import (
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/planner"
)

// TestApplyInFlight_CoversPostMoveJellyfinSync pins the window this used to
// get wrong: after the last move lands, startApply's goroutine still holds
// the mover lock while it re-resolves and refreshes each moved item in
// Jellyfin, which can take minutes. Keying "is an apply running" off the
// moves alone let the Plan page invite a click - and the scheduler build a
// whole plan - only to fail on AcquireLock with "another apply is already
// running", contradicting what the page had just said.
func TestApplyInFlight_CoversPostMoveJellyfinSync(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	srv := newTestServer(t, dir, hotDir, coldDir, "", "", false)

	// An empty plan finishes its (zero) moves immediately, which is exactly
	// the state under test: moves done, run not yet over.
	progress := (&mover.Movers{}).Apply(&planner.Plan{}, nil)
	progress.Wait()

	run := &applyRun{progress: progress}
	run.active.Store(true)
	srv.applyMu.Lock()
	srv.currentRun = run
	srv.applyMu.Unlock()

	if !progress.Snapshot().Done {
		t.Fatal("test setup: expected an empty plan's moves to be done")
	}
	if !srv.applyInFlight() {
		t.Fatal("a run still syncing Jellyfin must count as in flight - it still holds the mover lock")
	}

	status := srv.currentApplyStatus()
	if !status.Running || !status.SyncingJellyfin {
		t.Fatalf("expected the page to report the Jellyfin sync phase, got %+v", status)
	}

	run.active.Store(false)
	if srv.applyInFlight() {
		t.Fatal("a fully finished run must not count as in flight")
	}
	if status := srv.currentApplyStatus(); status.Running || status.SyncingJellyfin {
		t.Fatalf("expected a finished run to report neither running nor syncing, got %+v", status)
	}
}

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
