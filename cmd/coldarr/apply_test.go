package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/mover"
)

func TestReportAndPlan_AreReadOnly(t *testing.T) {
	cfgPath, hotDir, coldDir := testConfig(t)
	radarr := newFakeRadarr(t, hotDir)
	connectRadarr(t, cfgPath, radarr.URL)

	out, err := runColdarr(t, "--config", cfgPath, "report")
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	for _, want := range []string{"hot1", "cold1", "Inventory: 1 items (0 protected, 0 hot, 1 cold candidates)", "Movie A"} {
		if !strings.Contains(out, want) {
			t.Errorf("report output is missing %q:\n%s", want, out)
		}
	}

	out, err = runColdarr(t, "--config", cfgPath, "plan")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, want := range []string{"Movie A", "cold1 (" + coldDir + ")", "1 items", "Projected usage after plan"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output is missing %q:\n%s", want, out)
		}
	}

	if moves := radarr.movesRequested(); len(moves) != 0 {
		t.Fatalf("report and plan requested moves %v, want none", moves)
	}
}

func TestApply_MovesAfterConfirmation(t *testing.T) {
	cfgPath, hotDir, coldDir := testConfig(t)
	radarr := newFakeRadarr(t, hotDir)
	connectRadarr(t, cfgPath, radarr.URL)

	// Anything but y/yes at the prompt leaves everything alone.
	withStdin(t, "n\n")
	out, err := runColdarr(t, "--config", cfgPath, "apply")
	if err != nil || !strings.Contains(out, "Proceed with 1 move(s)? [y/N]") || !strings.Contains(out, "Aborted - no changes made.") {
		t.Fatalf("apply answered n = (%v):\n%s", err, out)
	}
	if moves := radarr.movesRequested(); len(moves) != 0 {
		t.Fatalf("an aborted apply requested moves %v", moves)
	}

	withStdin(t, "YES\n")
	out, err = runColdarr(t, "--config", cfgPath, "apply")
	if err != nil {
		t.Fatalf("apply answered YES: %v\n%s", err, out)
	}
	for _, want := range []string{"done:    Movie A", "Moved 1 item(s)."} {
		if !strings.Contains(out, want) {
			t.Errorf("apply output is missing %q:\n%s", want, out)
		}
	}
	if moves := radarr.movesRequested(); len(moves) != 1 || moves[0] != coldDir {
		t.Fatalf("moves requested = %v, want Movie A moved to %s", moves, coldDir)
	}
	hist, err := history.Load(filepath.Join(filepath.Dir(cfgPath), "history.json"))
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}
	if recs := hist.All(); len(recs) != 1 || recs[0].Title != "Movie A" || recs[0].ToTier != "cold1" {
		t.Fatalf("history = %+v, want the move recorded", recs)
	}

	// Movie A now sits on cold storage, so there's nothing left to do.
	out, err = runColdarr(t, "--config", cfgPath, "apply", "--yes")
	if err != nil || !strings.Contains(out, "Nothing to do.") {
		t.Fatalf("apply with nothing to move = (%v):\n%s", err, out)
	}
}

// TestApply_RefusesUnsafeStarts: apply checks everything that makes a run
// unsafe before it moves anything - moves still running inside Radarr
// (possibly from a Coldarr that has since died), a tier path that's gone,
// or another apply holding the lock.
func TestApply_RefusesUnsafeStarts(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, cfgPath, coldDir string, radarr *fakeRadarr)
		wantErr string
	}{
		{
			name: "moves still running in radarr",
			setup: func(t *testing.T, _, _ string, radarr *fakeRadarr) {
				radarr.mu.Lock()
				radarr.moving = 2
				radarr.mu.Unlock()
			},
			wantErr: "refusing to plan or apply: Radarr is still executing 2 move command(s)",
		},
		{
			name: "radarr can't say whether moves are running",
			setup: func(t *testing.T, _, _ string, radarr *fakeRadarr) {
				radarr.mu.Lock()
				radarr.commandsDown = true
				radarr.mu.Unlock()
			},
			wantErr: "checking radarr for in-flight moves",
		},
		{
			name: "a tier path is gone",
			setup: func(t *testing.T, _, coldDir string, _ *fakeRadarr) {
				if err := os.Remove(coldDir); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "storage unavailable",
		},
		{
			name: "another apply holds the lock",
			setup: func(t *testing.T, cfgPath, _ string, _ *fakeRadarr) {
				lock, err := mover.AcquireLock(filepath.Dir(cfgPath))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Release() })
			},
			wantErr: "another apply is already running",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath, hotDir, coldDir := testConfig(t)
			radarr := newFakeRadarr(t, hotDir)
			connectRadarr(t, cfgPath, radarr.URL)
			tt.setup(t, cfgPath, coldDir, radarr)

			if _, err := runColdarr(t, "--config", cfgPath, "apply", "--yes"); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("apply = %v, want an error containing %q", err, tt.wantErr)
			}
			if moves := radarr.movesRequested(); len(moves) != 0 {
				t.Fatalf("a refused apply requested moves %v", moves)
			}
		})
	}
}

func TestApply_FailedMovesFailTheCommand(t *testing.T) {
	cfgPath, hotDir, _ := testConfig(t)
	radarr := newFakeRadarr(t, hotDir)
	radarr.mu.Lock()
	radarr.failMoves = true
	radarr.mu.Unlock()
	connectRadarr(t, cfgPath, radarr.URL)

	out, err := runColdarr(t, "--config", cfgPath, "apply", "-y")
	if err == nil || err.Error() != "1 move(s) failed" {
		t.Fatalf("apply with a failing move = %v, want it to fail the command", err)
	}
	for _, want := range []string{"failed:  Movie A", "Moved 0 item(s).", "Failed to move 1 item(s):", "  - Movie A:"} {
		if !strings.Contains(out, want) {
			t.Errorf("apply output is missing %q:\n%s", want, out)
		}
	}
}

// TestApply_FinishesJellyfinsRefresh: with Jellyfin connected, apply
// reports each move to it and waits for Jellyfin to pick the moved item up
// at its new path before it exits.
func TestApply_FinishesJellyfinsRefresh(t *testing.T) {
	cfgPath, hotDir, coldDir := testConfig(t)
	radarr := newFakeRadarr(t, hotDir)
	connectRadarr(t, cfgPath, radarr.URL)
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_INTERVAL", "10ms")
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_TIMEOUT", "3s")

	var mu sync.Mutex
	var refreshed []string
	jellyfin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items" && r.URL.Query().Get("Filters") == "IsFavorite":
			_, _ = w.Write([]byte(`{"Items": []}`))
		case r.URL.Path == "/Users/u1/Items":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{
				"Id": "jf-movie-a", "Type": "Movie", "Path": filepath.Join(coldDir, "Movie A", "Movie A.mkv"),
			}}})
		case r.URL.Path == "/Library/Media/Updated":
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/Refresh"):
			refreshed = append(refreshed, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected jellyfin %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(jellyfin.Close)
	if _, err := runColdarr(t, "--config", cfgPath, "connections", "set", "jellyfin", "--url", jellyfin.URL, "--api-key", "test"); err != nil {
		t.Fatal(err)
	}

	out, err := runColdarr(t, "--config", cfgPath, "apply", "--yes")
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Finishing Jellyfin's refresh of the moved items...") {
		t.Errorf("apply should say it's waiting on Jellyfin:\n%s", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(refreshed) != 1 || refreshed[0] != "/Items/jf-movie-a/Refresh" {
		t.Fatalf("Jellyfin refreshes = %v, want the moved item refreshed at its new path", refreshed)
	}
}
