package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/planner"
	"github.com/vocoder/coldarr/internal/secrets"
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

	progress, err := srv.startApply(eng, inv, plan, false)
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

// TestStartApply_StartsUserDataRestoreOnceAtVeryEnd pins the scope and the
// ordering of the optional follow-up. Per-item Jellyfin move reports may happen
// while the plan runs, but the plugin task itself is started exactly once,
// after the whole plan has drained and the final moved item was resolved and
// refreshed at its new path.
func TestStartApply_StartsUserDataRestoreOnceAtVeryEnd(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)

	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	jellyfinServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/Users":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "user-1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-1/Items":
			if r.URL.Query().Get("Filters") == "IsFavorite" {
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{
				"Id": "moved-item-id", "Path": filepath.Join(coldDir, "Movie A", "Movie A.mkv"), "Type": "Movie",
			}}})
		case r.Method == http.MethodPost && r.URL.Path == "/Library/Media/Updated":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/Items/moved-item-id/Refresh":
			record("confirmed")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/ScheduledTasks":
			record("task-resolved")
			_, _ = w.Write([]byte(`[{"Id":"restore-runtime-id","Key":"UserDataRestore","State":"Idle"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/ScheduledTasks/Running/restore-runtime-id":
			record("task-started")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Jellyfin request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(jellyfinServer.Close)
	if err := srv.connStore.Set("jellyfin", secrets.Connection{URL: jellyfinServer.URL, APIKey: "test", Enabled: true}); err != nil {
		t.Fatalf("connStore.Set jellyfin: %v", err)
	}

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
	if len(plan.Entries) != 1 {
		t.Fatalf("test setup: plan entries = %d, want 1", len(plan.Entries))
	}

	if _, err := srv.startApply(eng, inv, plan, true); err != nil {
		t.Fatalf("startApply: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for srv.applyInFlight() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.applyInFlight() {
		t.Fatal("apply did not finish")
	}

	mu.Lock()
	got := strings.Join(events, ",")
	mu.Unlock()
	if got != "confirmed,task-resolved,task-started" {
		t.Fatalf("Jellyfin events = %q, want confirmation then exactly one task resolution/start", got)
	}
}

// planOffersApply reports whether a rendered /plan page shows a plan to
// act on: the item count under the table, or the Apply form itself.
func planOffersApply(body string) bool {
	return strings.Contains(body, `action="/plan/apply"`) || strings.Contains(body, "item(s)")
}

// TestPlanPage_RefusalShowsNoPlan pins that a refused plan is only the
// refusal. Every refusal left Plan.Empty false with no entries, so the page
// fell through to the has-a-plan branch: an empty table, "0 item(s), total"
// and an Apply button beneath the error saying nothing may move.
func TestPlanPage_RefusalShowsNoPlan(t *testing.T) {
	cases := []struct {
		name    string
		breakIt func(t *testing.T, radarr *fakeRadarr, coldDir string)
		want    string // in the rendered refusal
	}{
		{
			name: "healthy plan still offers apply",
		},
		{
			name: "drive missing",
			breakIt: func(t *testing.T, _ *fakeRadarr, coldDir string) {
				if err := os.RemoveAll(coldDir); err != nil {
					t.Fatalf("removing cold tier: %v", err)
				}
			},
			want: "storage unavailable, refusing to move anything",
		},
		{
			name: "moves still in flight",
			breakIt: func(_ *testing.T, radarr *fakeRadarr, _ string) {
				radarr.setActiveMoveCommands(1)
			},
			want: "Radarr is still executing 1 move command(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, hotDir, coldDir := testTierDirs(t)
			radarr := newFakeRadarr(t, hotDir)
			srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)
			if tc.breakIt != nil {
				tc.breakIt(t, radarr, coldDir)
			}

			rec := httptest.NewRecorder()
			srv.handlePlanPage(rec, httptest.NewRequest(http.MethodGet, "/plan", nil))
			body := rec.Body.String()

			if tc.want == "" {
				if !planOffersApply(body) || !strings.Contains(body, "Movie A") {
					t.Fatalf("healthy plan should list Movie A and offer Apply, got:\n%s", body)
				}
				return
			}
			if !strings.Contains(body, tc.want) {
				t.Fatalf("page should show the refusal %q, got:\n%s", tc.want, body)
			}
			if planOffersApply(body) {
				t.Fatalf("refused plan still renders a plan table or Apply button:\n%s", body)
			}
			if strings.Contains(body, "nothing moves until you click Apply") {
				t.Fatalf("refused plan still says it's a dry run awaiting Apply:\n%s", body)
			}
		})
	}
}

// TestApplyStart_RefusalShowsNoPlan covers the other way onto a refused
// plan: an Apply that startApply turns down re-renders /plan with the
// error (renderPlanError), which must not offer the same Apply again.
func TestApplyStart_RefusalShowsNoPlan(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)
	radarr.setActiveMoveCommands(1)

	rec := httptest.NewRecorder()
	srv.handleApplyStart(rec, httptest.NewRequest(http.MethodPost, "/plan/apply", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "refusing to apply") {
		t.Fatalf("apply should have been refused, got %d:\n%s", rec.Code, body)
	}
	if planOffersApply(body) {
		t.Fatalf("refused apply re-renders a plan table or Apply button:\n%s", body)
	}
	if len(radarr.calls()) != 0 {
		t.Fatalf("refused apply still asked Radarr to move: %v", radarr.calls())
	}
}

// TestApplyStart_RunsThePlanInTheBackground: the Apply button starts the
// moves and returns to /plan at once; the status partial keeps polling
// while the run is active, then has htmx reload the whole page once it's
// over.
func TestApplyStart_RunsThePlanInTheBackground(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)

	rec := serveForm(srv.handleApplyStart, "/plan/apply", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/plan" {
		t.Fatalf("POST /plan/apply = %d %q, want a redirect to /plan:\n%s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	waitApplyIdle(t, srv)

	if calls := radarr.calls(); len(calls) != 1 || calls[0]["rootFolderPath"] != coldDir {
		t.Fatalf("radarr editor calls = %v, want Movie A moved to %s", calls, coldDir)
	}
	status := srv.currentApplyStatus()
	if status.Running || status.MovedCount != 1 || status.FailedCount != 0 || status.Finished == "" {
		t.Fatalf("status = %+v, want a finished run with one move", status)
	}

	partial := httptest.NewRecorder()
	srv.handleApplyStatusPartial(partial, httptest.NewRequest(http.MethodGet, "/plan/apply/status/partial", nil))
	if partial.Header().Get("HX-Refresh") != "true" {
		t.Error("a finished run's partial should tell htmx to reload the page")
	}

	// While a run is active, the partial just updates in place.
	running := &applyRun{progress: (&mover.Movers{}).Apply(&planner.Plan{}, nil)}
	running.active.Store(true)
	srv.applyMu.Lock()
	srv.currentRun = running
	srv.applyMu.Unlock()
	partial = httptest.NewRecorder()
	srv.handleApplyStatusPartial(partial, httptest.NewRequest(http.MethodGet, "/plan/apply/status/partial", nil))
	if partial.Header().Get("HX-Refresh") != "" {
		t.Error("an active run's partial must not reload the page")
	}

	// And a second click while it's active doesn't start anything.
	rec = serveForm(srv.handleApplyStart, "/plan/apply", nil)
	if rec.Code != http.StatusSeeOther || len(radarr.calls()) != 1 {
		t.Fatalf("POST /plan/apply mid-run = %d with %d moves, want a redirect and no new moves", rec.Code, len(radarr.calls()))
	}
}

func TestApplyStart_NothingToApply(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, coldDir) // already on cold storage
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)

	rec := serveForm(srv.handleApplyStart, "/plan/apply", nil)
	if rec.Code != http.StatusSeeOther || srv.currentRun != nil {
		t.Fatalf("POST /plan/apply with nothing to move = %d (run %v), want a redirect and no run", rec.Code, srv.currentRun)
	}
}

// TestPlanAndApply_ShowWhyThereIsNoPlan: every way a plan can't be built is
// shown in the plan section of /plan, whether from viewing it or from
// clicking Apply.
func TestPlanAndApply_ShowWhyThereIsNoPlan(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, srv *Server, base *fakeRadarr) string // returns the Radarr URL to use
		wantErr string
	}{
		{
			name: "unreadable history",
			setup: func(t *testing.T, srv *Server, base *fakeRadarr) string {
				if err := os.WriteFile(srv.cfg.History.Path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
				return base.URL
			},
			wantErr: "parsing history",
		},
		{
			name: "radarr can't report its moves",
			setup: func(t *testing.T, srv *Server, base *fakeRadarr) string {
				return radarrWith(t, base, map[string]http.HandlerFunc{"GET /api/v3/command": fail500}).URL
			},
			wantErr: "checking radarr for in-flight moves",
		},
		{
			name: "radarr library unreadable",
			setup: func(t *testing.T, srv *Server, base *fakeRadarr) string {
				return radarrWith(t, base, map[string]http.HandlerFunc{"GET /api/v3/movie": fail500}).URL
			},
			wantErr: "fetching movies from radarr",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, hotDir, coldDir := testTierDirs(t)
			base := newFakeRadarr(t, hotDir)
			srv := newTestServer(t, dir, hotDir, coldDir, base.URL, "", false)
			if err := srv.connStore.Set("radarr", secretsConn(tt.setup(t, srv, base))); err != nil {
				t.Fatal(err)
			}

			if data := srv.buildPlanData(); !strings.Contains(data.Error, tt.wantErr) || len(data.Entries) != 0 {
				t.Errorf("plan data = %+v, want no entries and an error containing %q", data, tt.wantErr)
			}
			rec := serveForm(srv.handleApplyStart, "/plan/apply", nil)
			if body := rec.Body.String(); !strings.Contains(body, tt.wantErr) || planOffersApply(body) {
				t.Errorf("Apply should show %q and no plan:\n%s", tt.wantErr, body)
			}
			if len(base.calls()) != 0 {
				t.Error("nothing may move when the plan couldn't be built")
			}
		})
	}
}

// TestStartApply_JellyfinTroubleNeverBlocksTheMoves: Jellyfin only follows
// the moves. Failing to read its dates before the run, or to confirm the
// moved items after it, never stops the moves themselves - but the
// user-data restore follow-up waits on that confirmation, so it isn't
// started.
func TestStartApply_JellyfinTroubleNeverBlocksTheMoves(t *testing.T) {
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_TIMEOUT", "50ms")
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_INTERVAL", "5ms")
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)

	var mu sync.Mutex
	var scans, taskRequests int
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items" && r.URL.Query().Get("Filters") == "IsFavorite":
			_, _ = w.Write([]byte(`{"Items": []}`))
		case r.URL.Path == "/Users/u1/Items":
			http.Error(w, "library busy", http.StatusServiceUnavailable)
		case r.URL.Path == "/Library/Media/Updated":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/Library/Refresh":
			scans++
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/ScheduledTasks"):
			taskRequests++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected jellyfin %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(jf.Close)
	if err := srv.connStore.Set("jellyfin", secrets.Connection{URL: jf.URL, APIKey: "test", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	startPlannedApply(t, srv, true)
	waitApplyIdle(t, srv)

	if calls := radarr.calls(); len(calls) != 1 {
		t.Fatalf("radarr editor calls = %d, want the move made regardless of Jellyfin", len(calls))
	}
	mu.Lock()
	defer mu.Unlock()
	if scans != 1 {
		t.Errorf("library scans = %d, want the whole-library fallback once", scans)
	}
	if taskRequests != 0 {
		t.Errorf("scheduled-task requests = %d, want the restore follow-up withheld", taskRequests)
	}
}

// TestStartApply_RestoreFollowUpProblemsDontWedgeTheRun: asking for the
// user-data restore without Jellyfin, or with a Jellyfin that won't start
// it, still finishes the run and frees the lock for the next one.
func TestStartApply_RestoreFollowUpProblemsDontWedgeTheRun(t *testing.T) {
	for _, withJellyfin := range []bool{false, true} {
		name := "no jellyfin"
		if withJellyfin {
			name = "task won't start"
		}
		t.Run(name, func(t *testing.T) {
			dir, hotDir, coldDir := testTierDirs(t)
			radarr := newFakeRadarr(t, hotDir)
			srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)

			var mu sync.Mutex
			taskRequests := 0
			if withJellyfin {
				jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case r.URL.Path == "/Users":
						_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
					case r.URL.Path == "/Users/u1/Items" && r.URL.Query().Get("Filters") == "IsFavorite":
						_, _ = w.Write([]byte(`{"Items": []}`))
					case r.URL.Path == "/Users/u1/Items":
						_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{
							"Id": "moved", "Type": "Movie", "Path": filepath.Join(coldDir, "Movie A", "Movie A.mkv"),
						}}})
					case r.URL.Path == "/Library/Media/Updated", r.URL.Path == "/Items/moved/Refresh":
						w.WriteHeader(http.StatusNoContent)
					case r.URL.Path == "/ScheduledTasks":
						taskRequests++
						http.Error(w, "plugin crashed", http.StatusInternalServerError)
					default:
						t.Errorf("unexpected jellyfin %s %s", r.Method, r.URL.Path)
					}
				}))
				t.Cleanup(jf.Close)
				if err := srv.connStore.Set("jellyfin", secrets.Connection{URL: jf.URL, APIKey: "test", Enabled: true}); err != nil {
					t.Fatal(err)
				}
			}

			startPlannedApply(t, srv, true)
			waitApplyIdle(t, srv)

			if len(radarr.calls()) != 1 {
				t.Fatal("expected the move made")
			}
			mu.Lock()
			defer mu.Unlock()
			if withJellyfin && taskRequests != 1 {
				t.Errorf("scheduled-task lookups = %d, want the one attempt", taskRequests)
			}
			lock, err := mover.AcquireLock(filepath.Dir(srv.cfgPath))
			if err != nil {
				t.Fatalf("the finished run left the mover lock held: %v", err)
			}
			_ = lock.Release()
		})
	}
}

// startPlannedApply builds the current plan and starts applying it, the way
// both the Apply button and the scheduler do.
func startPlannedApply(t *testing.T, srv *Server, startUserDataRestore bool) {
	t.Helper()
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
	if len(plan.Entries) != 1 {
		t.Fatalf("test setup: plan entries = %d, want 1", len(plan.Entries))
	}
	if _, err := srv.startApply(eng, inv, plan, startUserDataRestore); err != nil {
		t.Fatalf("startApply: %v", err)
	}
}

func TestCurrentApplyStatus_CountsFailedMoves(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	base := newFakeRadarr(t, hotDir)
	radarr := radarrWith(t, base, map[string]http.HandlerFunc{"PUT /api/v3/movie/editor": fail500})
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)

	startPlannedApply(t, srv, false)
	waitApplyIdle(t, srv)

	status := srv.currentApplyStatus()
	if status.FailedCount != 1 || status.MovedCount != 0 || len(status.Entries) != 1 || status.Entries[0].Status != string(mover.StatusFailed) || !strings.Contains(status.Entries[0].Err, "500") {
		t.Fatalf("status = %+v, want the one failed move with its reason", status)
	}
}
