package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/scheduler"
	"github.com/vocoder/coldarr/internal/secrets"
)

// TestHandleSchedulerSave_RejectedScheduleKeepsWhatWasTyped: an invalid
// schedule is refused for that task alone. Its form comes back with the
// error and everything typed into it - including Run the Plan's follow-up
// checkbox and the run-now buttons the other tasks carry - while the saved
// schedule stays as it was.
func TestHandleSchedulerSave_RejectedScheduleKeepsWhatWasTyped(t *testing.T) {
	for _, task := range scheduledTasks {
		t.Run(task, func(t *testing.T) {
			srv := newSettingsTestServer(t)
			before := scheduleFor(srv.currentConfig(), task)

			page := httptest.NewRecorder()
			srv.handleSchedulerPage(page, httptest.NewRequest(http.MethodGet, "/settings/scheduler", nil))

			form := url.Values{"enabled": {"on"}, "unit": {"daily"}, "every": {"2"}, "at": {"25:00"}}
			if task == taskRunPlan {
				form.Set("start_userdata_restore_after_move", "on")
			}
			body := serveForm(srv.handleSchedulerSave, "/settings/scheduler/"+task, form, "task", task).Body.String()

			if !strings.Contains(body, "at must be in HH:MM form") {
				t.Fatalf("expected the validation error:\n%s", body)
			}
			if !strings.Contains(body, `id="at-`+task+`" name="at" value="25:00"`) || !strings.Contains(body, `id="every-`+task+`" name="every" min="1" value="2"`) {
				t.Errorf("the rejected form should keep what was typed:\n%s", body)
			}
			if task == taskRunPlan && !strings.Contains(body, `name="start_userdata_restore_after_move" checked`) {
				t.Error("the rejected Run the Plan form should keep its follow-up checkbox")
			}
			for _, button := range []string{"Refresh now", "Scan now"} {
				if want, got := strings.Count(page.Body.String(), button), strings.Count(body, button); got != want {
					t.Errorf("%q buttons = %d after the rejected save, want the page's usual %d", button, got, want)
				}
			}
			if got := scheduleFor(srv.currentConfig(), task); got != before {
				t.Errorf("schedule = %+v after a rejected save, want it left as %+v", got, before)
			}
		})
	}
}

func TestHandleSchedulerSave_SavesEachTask(t *testing.T) {
	for _, task := range scheduledTasks {
		t.Run(task, func(t *testing.T) {
			srv := newSettingsTestServer(t)
			body := serveForm(srv.handleSchedulerSave, "/settings/scheduler/"+task,
				url.Values{"enabled": {"on"}, "unit": {"hourly"}, "every": {"6"}}, "task", task).Body.String()
			if !strings.Contains(body, "Schedule saved.") {
				t.Fatalf("save did not report success:\n%s", body)
			}

			reloaded, err := config.LoadForServer(srv.cfgPath)
			if err != nil {
				t.Fatalf("reloading saved config: %v", err)
			}
			for where, got := range map[string]scheduler.Schedule{"live": scheduleFor(srv.currentConfig(), task), "saved": scheduleFor(reloaded, task)} {
				if !got.Enabled || got.Unit != scheduler.Hourly || got.Every != 6 {
					t.Errorf("%s %s schedule = %+v, want enabled every 6 hours", where, task, got)
				}
			}
		})
	}
}

func TestHandleSchedulerSave_RejectsUnknownTasksAndDays(t *testing.T) {
	srv := newSettingsTestServer(t)

	if rec := serveForm(srv.handleSchedulerSave, "/settings/scheduler/defrag", url.Values{"enabled": {"on"}}, "task", "defrag"); rec.Code != http.StatusNotFound {
		t.Errorf("saving an unknown task = %d, want 404", rec.Code)
	}

	body := serveForm(srv.handleSchedulerSave, "/settings/scheduler/omit-days", url.Values{"omit_days": {"saturday", "funday"}}, "task", "omit-days").Body.String()
	if !strings.Contains(body, `unknown weekday`) {
		t.Fatalf("expected the bad weekday refused:\n%s", body)
	}
	if days := srv.currentConfig().Scheduler.WeeklyOmitDays; len(days) != 0 {
		t.Errorf("WeeklyOmitDays = %v after a rejected save, want none", days)
	}
}

// failingArr answers every Radarr/Sonarr request with a 500.
func failingArr(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunNowButtons: each manual trigger runs its task on the spot and
// counts as a run when it succeeds, or shows the failure on that task's own
// form - and the quality-cutoff scan refuses to run alongside another.
func TestRunNowButtons(t *testing.T) {
	srv := newSettingsTestServer(t)

	if body := serveForm(srv.handleRefreshLinksNow, "/settings/scheduler/refresh_links/run", nil).Body.String(); !strings.Contains(body, "Links cache refreshed.") {
		t.Fatalf("refresh with nothing configured should succeed:\n%s", body)
	}
	if srv.getLastRanRefreshLinks().IsZero() {
		t.Error("a manual refresh should count as the task's last run")
	}

	srv.scanCutoffsMu.Lock()
	body := serveForm(srv.handleScanCutoffsNow, "/settings/scheduler/scan_cutoffs/run", nil).Body.String()
	srv.scanCutoffsMu.Unlock()
	if !strings.Contains(body, "A scan is already in progress") {
		t.Fatalf("expected an overlapping cutoff scan refused:\n%s", body)
	}

	if err := srv.connStore.Set("radarr", secretsConn(failingArr(t).URL)); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		handler http.HandlerFunc
		wantErr string
	}{
		"refresh links": {srv.handleRefreshLinksNow, "fetching radarr link targets"},
		"scan cutoffs":  {srv.handleScanCutoffsNow, "fetching radarr cutoff-unmet movie IDs"},
		"scan orphans":  {srv.handleScanOrphansNow, "fetching radarr movies"},
	} {
		if body := serveForm(c.handler, "/settings/scheduler/run", nil).Body.String(); !strings.Contains(body, c.wantErr) || strings.Contains(body, "alert-ok") {
			t.Errorf("%s with Radarr failing should show %q:\n%s", name, c.wantErr, body)
		}
	}
	if !srv.getLastRanScanCutoffs().IsZero() || !srv.getLastRanScanOrphans().IsZero() {
		t.Error("a failed manual run must not count as the task's last run")
	}
}

// waitForNotifications waits until apprise has received at least n
// notifications and returns them.
func waitForNotifications(t *testing.T, apprise *fakeApprise, n int) []map[string]string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := apprise.notifications()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifications = %v, want at least %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitApplyIdle waits for a background apply to let go of the mover lock,
// so it can't outlive the test that started it.
func waitApplyIdle(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for srv.applyInFlight() {
		if time.Now().After(deadline) {
			t.Fatal("apply still in flight")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestScheduledTasks_ReportFailuresByNotification: nobody is watching a
// page when a scheduled task runs, so each failure goes out as a failure
// notification - and still counts as the task's run, so a broken setup
// isn't retried (and re-notified) every minute.
func TestScheduledTasks_ReportFailuresByNotification(t *testing.T) {
	tests := []struct {
		name      string
		run       func(*Server, time.Time)
		lastRan   func(*Server) time.Time
		wantTitle string
	}{
		{"rescan cold storage", (*Server).runScheduledRescan, (*Server).getLastRanRescan, "Cold storage check failed"},
		{"refresh links", (*Server).runScheduledRefreshLinks, (*Server).getLastRanRefreshLinks, "Links cache refresh failed"},
		{"scan cutoffs", (*Server).runScheduledScanCutoffs, (*Server).getLastRanScanCutoffs, "Quality-cutoff scan failed"},
		{"scan orphans", (*Server).runScheduledScanOrphans, (*Server).getLastRanScanOrphans, "Orphaned storage scan failed"},
	}
	for _, cause := range []string{"arr unreachable", "history unreadable"} {
		for _, tt := range tests {
			t.Run(cause+"/"+tt.name, func(t *testing.T) {
				dir, hotDir, coldDir := testTierDirs(t)
				apprise := newFakeApprise(t)
				radarrURL := newFakeRadarr(t, hotDir).URL
				if cause == "arr unreachable" {
					radarrURL = failingArr(t).URL
				}
				srv := newTestServer(t, dir, hotDir, coldDir, radarrURL, apprise.URL, false)
				if cause == "history unreadable" {
					if err := os.WriteFile(srv.cfg.History.Path, []byte("{"), 0o600); err != nil {
						t.Fatal(err)
					}
				}

				now := time.Now()
				tt.run(srv, now)

				got := waitForNotifications(t, apprise, 1)
				if len(got) != 1 || got[0]["title"] != tt.wantTitle || got[0]["type"] != "failure" {
					t.Fatalf("notifications = %v, want one %q failure", got, tt.wantTitle)
				}
				if ran := tt.lastRan(srv); !ran.Equal(now) {
					t.Errorf("last ran = %v, want the failed run recorded at %v", ran, now)
				}
			})
		}
	}
}

// TestScheduledTasks_NeverOverlapThemselves: a tick that finds the same
// task's previous run still going skips it entirely - no second run, no
// notification, and nothing recorded as a run.
func TestScheduledTasks_NeverOverlapThemselves(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	apprise := newFakeApprise(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, apprise.URL, false)

	for name, task := range map[string]struct {
		mu      *sync.Mutex
		run     func(time.Time)
		lastRan func() time.Time
	}{
		"rescan cold storage": {&srv.rescanMu, srv.runScheduledRescan, srv.getLastRanRescan},
		"refresh links":       {&srv.refreshLinksMu, srv.runScheduledRefreshLinks, srv.getLastRanRefreshLinks},
		"scan cutoffs":        {&srv.scanCutoffsMu, srv.runScheduledScanCutoffs, srv.getLastRanScanCutoffs},
		"scan orphans":        {&srv.scanOrphansMu, srv.runScheduledScanOrphans, srv.getLastRanScanOrphans},
	} {
		task.mu.Lock()
		task.run(time.Now())
		task.mu.Unlock()
		if !task.lastRan().IsZero() {
			t.Errorf("%s: a skipped tick was recorded as a run", name)
		}
	}
	if got := apprise.notifications(); len(got) != 0 {
		t.Errorf("notifications = %v, want none from skipped ticks", got)
	}
	if radarr.cutoffHits() != 0 {
		t.Error("a skipped cutoff scan must not reach Radarr")
	}
}

func TestRunScheduledRescan_ReportsEachColdPath(t *testing.T) {
	t.Run("a cold path is unavailable", func(t *testing.T) {
		dir, hotDir, coldDir := testTierDirs(t)
		apprise := newFakeApprise(t)
		srv := newTestServer(t, dir, hotDir, coldDir, "", apprise.URL, true)
		srv.cfg.Tiers = append(srv.cfg.Tiers, model.Tier{
			Name: "sat1", Role: model.RoleCold, Paths: []string{filepath.Join(dir, "unplugged")}, Media: []model.MediaType{model.Movie},
			MaxUsedPercent: 95, TargetUsedPercent: 90,
		})

		srv.runScheduledRescan(time.Now())

		// One per-path item for each cold path (verbose), then the summary.
		got := waitForNotifications(t, apprise, 3)
		var summary, unavailable map[string]string
		for _, n := range got {
			switch n["title"] {
			case "Cold storage check: 1 issue(s)":
				summary = n
			case "sat1 unavailable":
				unavailable = n
			}
		}
		if summary == nil || summary["type"] != "warning" || !strings.Contains(summary["body"], "does not exist") || !strings.Contains(summary["body"], "used") {
			t.Errorf("summary = %v, want a warning listing the healthy path's usage and sat1's failure", summary)
		}
		if unavailable == nil || unavailable["type"] != "failure" {
			t.Errorf("notifications = %v, want a failure item for sat1", got)
		}
	})

	t.Run("no cold tiers", func(t *testing.T) {
		dir, hotDir, coldDir := testTierDirs(t)
		apprise := newFakeApprise(t)
		srv := newTestServer(t, dir, hotDir, coldDir, "", apprise.URL, false)
		srv.cfg.Tiers = srv.cfg.Tiers[:1]

		srv.runScheduledRescan(time.Now())

		got := waitForNotifications(t, apprise, 1)
		if got[0]["title"] != "Cold storage check finished" || got[0]["body"] != "No cold tiers configured." {
			t.Fatalf("notification = %v, want the no-cold-tiers summary", got[0])
		}
	})
}

// radarrWith serves overrides itself - keyed "METHOD /path" - and every
// other request from base.
func radarrWith(t *testing.T, base *fakeRadarr, overrides map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := overrides[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		base.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fail500(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "boom", http.StatusInternalServerError)
}

func TestRunScheduledPlan_NothingToMove(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	apprise := newFakeApprise(t)
	radarr := newFakeRadarr(t, coldDir) // already on cold storage
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, apprise.URL, false)

	now := time.Now()
	srv.runScheduledPlan(now)

	got := waitForNotifications(t, apprise, 1)
	if got[0]["title"] != "Scheduled apply: nothing to move" || got[0]["type"] != "info" {
		t.Fatalf("notification = %v, want the nothing-to-move summary", got[0])
	}
	if !srv.getLastRanPlan().Equal(now) {
		t.Error("an empty plan still counts as the run")
	}
	if len(radarr.calls()) != 0 {
		t.Error("nothing should have been moved")
	}
}

// TestRunScheduledPlan_UnknownArrStatusSkipsTheTick: if Radarr can't say
// whether earlier moves are still running, the tick does nothing - no plan,
// no notification - and isn't recorded, so the next tick tries again.
func TestRunScheduledPlan_UnknownArrStatusSkipsTheTick(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	apprise := newFakeApprise(t)
	base := newFakeRadarr(t, hotDir)
	radarr := radarrWith(t, base, map[string]http.HandlerFunc{"GET /api/v3/command": fail500})
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, apprise.URL, false)

	srv.runScheduledPlan(time.Now())

	if !srv.getLastRanPlan().IsZero() {
		t.Error("a skipped tick must not be recorded as a run")
	}
	if got := apprise.notifications(); len(got) != 0 {
		t.Errorf("notifications = %v, want none", got)
	}
	if len(base.calls()) != 0 || base.cutoffHits() != 0 {
		t.Error("nothing may be scanned or moved while Radarr's move status is unknown")
	}
}

// TestRunScheduledPlan_LockHeldElsewhereSkipsTheTick: the mover lock held
// by another apply (the CLI, say) means this tick skips without recording a
// run, so it retries once that apply is done.
func TestRunScheduledPlan_LockHeldElsewhereSkipsTheTick(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	apprise := newFakeApprise(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, apprise.URL, false)

	lock, err := mover.AcquireLock(filepath.Dir(srv.cfgPath))
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer func() { _ = lock.Release() }()

	srv.runScheduledPlan(time.Now())

	if !srv.getLastRanPlan().IsZero() {
		t.Error("a tick that couldn't take the lock must not be recorded as a run")
	}
	if len(radarr.calls()) != 0 {
		t.Error("nothing may move while another apply holds the lock")
	}
	if got := apprise.notifications(); len(got) != 0 {
		t.Errorf("notifications = %v, want none", got)
	}
}

// TestRunScheduledPlan_CutoffPreScanIsBestEffort: the plan's own
// quality-cutoff scan is a freshness bonus, not a precondition - whether
// another scan holds it or it fails, the plan still runs on cached data.
func TestRunScheduledPlan_CutoffPreScanIsBestEffort(t *testing.T) {
	for _, why := range []string{"scan already running", "scan fails"} {
		t.Run(why, func(t *testing.T) {
			dir, hotDir, coldDir := testTierDirs(t)
			apprise := newFakeApprise(t)
			base := newFakeRadarr(t, hotDir)
			radarr := radarrWith(t, base, map[string]http.HandlerFunc{"GET /api/v3/wanted/cutoff": fail500})
			srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, apprise.URL, false)

			if why == "scan already running" {
				srv.scanCutoffsMu.Lock()
				defer srv.scanCutoffsMu.Unlock()
			}
			srv.runScheduledPlan(time.Now())

			got := waitForNotifications(t, apprise, 1)
			if got[0]["title"] != "Scheduled apply finished" || !strings.Contains(got[0]["body"], "moved 1, failed 0") {
				t.Fatalf("notification = %v, want the plan applied anyway", got[0])
			}
			if !srv.getLastRanScanCutoffs().IsZero() {
				t.Error("a pre-scan that didn't complete must not count as a cutoff scan")
			}
			waitApplyIdle(t, srv)
		})
	}
}

func TestRunScheduledPlan_ReportsFailedMoves(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	apprise := newFakeApprise(t)
	base := newFakeRadarr(t, hotDir)
	radarr := radarrWith(t, base, map[string]http.HandlerFunc{"PUT /api/v3/movie/editor": fail500})
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, apprise.URL, true)

	srv.runScheduledPlan(time.Now())

	got := waitForNotifications(t, apprise, 2)
	if got[0]["title"] != "Scheduled apply finished" || got[0]["type"] != "warning" || !strings.Contains(got[0]["body"], "moved 0, failed 1") {
		t.Fatalf("summary = %v, want a warning counting the failed move", got[0])
	}
	if got[1]["title"] != "Failed to move Movie A" || got[1]["type"] != "failure" || !strings.Contains(got[1]["body"], "500") {
		t.Fatalf("item = %v, want Movie A's failure with the reason", got[1])
	}
	waitApplyIdle(t, srv)
}

func TestRunScheduledPlan_UnreadableHistoryIsReported(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	apprise := newFakeApprise(t)
	srv := newTestServer(t, dir, hotDir, coldDir, "", apprise.URL, false)
	if err := os.WriteFile(srv.cfg.History.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	srv.runScheduledPlan(now)

	got := waitForNotifications(t, apprise, 1)
	if got[0]["title"] != "Scheduled apply failed" || !strings.Contains(got[0]["body"], "parsing history") {
		t.Fatalf("notification = %v, want the failure reported", got[0])
	}
	if !srv.getLastRanPlan().Equal(now) {
		t.Error("a failed run still counts as the run")
	}
}

// secretsConn is a configured Radarr/Sonarr connection to url.
func secretsConn(url string) secrets.Connection {
	return secrets.Connection{URL: url, APIKey: "test", Enabled: true}
}
