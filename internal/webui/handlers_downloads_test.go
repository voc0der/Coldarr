package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/scheduler"
	"github.com/vocoder/coldarr/internal/secrets"
)

// TestDownloads_PlanThenMove walks the downloads lifecycle through the GUI:
// the dry run lists what's ready, Move moves it - really, on disk - one at
// a time, and only once each has landed does it get the seed label.
func TestDownloads_PlanThenMove(t *testing.T) {
	srv := newDownloadsTestServer(t)
	srv.qb.add(t, finishedDownload("aaa", "Linux.ISO", "manual", srv.ssd))
	srv.qb.add(t, finishedDownload("bbb", "Movie.2020", "movies", srv.ssd))
	srv.qb.add(t, finishedDownload("ccc", "Movie.2020", "manual", srv.ssd)) // a cross-seed of bbb: same files
	srv.radarr.setImported("bbb")
	srv.qb.add(t, finishedDownload("ddd", "Not.Imported", "movies", srv.ssd))

	page := httptest.NewRecorder()
	srv.handleDownloadsPage(page, httptest.NewRequest(http.MethodGet, "/downloads", nil))
	body := page.Body.String()
	for _, want := range []string{"Linux.ISO", "Movie.2020", "+1 cross-seed", "Move these downloads", "Held back", "Radarr hasn&#39;t imported it yet"} {
		if !strings.Contains(body, want) {
			t.Errorf("dry run is missing %q", want)
		}
	}
	var planned []string
	for _, m := range srv.buildDownloadsPageData().Plan.Moves {
		planned = append(planned, m.Name)
	}
	if strings.Join(planned, ",") != "Linux.ISO,Movie.2020" {
		t.Errorf("planned = %v, want Not.Imported left out - its importer hasn't imported it", planned)
	}

	rec := serveForm(srv.handleDownloadsApplyStart, "/downloads/apply", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("apply = %d:\n%s", rec.Code, rec.Body.String())
	}
	waitDownloadsIdle(t, srv.Server)

	for _, h := range []string{"aaa", "bbb", "ccc"} {
		got := srv.qb.torrent(h)
		if got.SavePath != srv.seed || got.Category != "seed" {
			t.Errorf("%s = %+v, want it on seed storage with the seed label", h, got)
		}
	}
	if got := srv.qb.torrent("ddd"); got.SavePath != srv.ssd || got.Category != "movies" {
		t.Errorf("ddd = %+v, want it left alone", got)
	}
	if _, err := os.Stat(filepath.Join(srv.seed, "Linux.ISO", "data.bin")); err != nil {
		t.Errorf("Linux.ISO isn't on seed storage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.ssd, "Movie.2020")); !os.IsNotExist(err) {
		t.Error("Movie.2020 should have left the download drive")
	}

	// The seed label is set only after each setLocation, never before.
	var order []string
	for _, p := range srv.qb.postedPaths() {
		order = append(order, strings.SplitN(p, "?", 2)[0])
	}
	if got := strings.Join(order, ","); got != "setLocation,createCategory,setCategory,setLocation,setCategory" {
		t.Errorf("writes = %s, want each move followed by its relabel, the pair in one request", got)
	}

	page = httptest.NewRecorder()
	srv.handleDownloadsPage(page, httptest.NewRequest(http.MethodGet, "/downloads", nil))
	body = page.Body.String()
	for _, want := range []string{"Last run", "2 moved, 0 failed", "Recent activity", "Linux.ISO"} {
		if !strings.Contains(body, want) {
			t.Errorf("after the run, the page is missing %q", want)
		}
	}

	// A status poll after the run tells htmx to reload the whole page.
	poll := httptest.NewRecorder()
	srv.handleDownloadsApplyStatusPartial(poll, httptest.NewRequest(http.MethodGet, "/downloads/apply/status/partial", nil))
	if poll.Header().Get("HX-Refresh") != "true" {
		t.Error("a finished run's status poll should refresh the page")
	}
}

// TestDownloads_RelabelOnly: a download already on seed storage that still
// has a download label - say Coldarr stopped between its move and its
// relabel - just gets the seed label.
func TestDownloads_RelabelOnly(t *testing.T) {
	srv := newDownloadsTestServer(t)
	srv.qb.add(t, finishedDownload("aaa", "Parked.ISO", "manual", srv.seed))

	page := httptest.NewRecorder()
	srv.handleDownloadsPage(page, httptest.NewRequest(http.MethodGet, "/downloads", nil))
	if !strings.Contains(page.Body.String(), "Relabel only") {
		t.Fatalf("the relabel should be offered:\n%s", page.Body.String())
	}
	serveForm(srv.handleDownloadsApplyStart, "/downloads/apply", nil)
	waitDownloadsIdle(t, srv.Server)
	if got := srv.qb.torrent("aaa"); got.Category != "seed" || got.SavePath != srv.seed {
		t.Errorf("aaa = %+v, want it relabeled in place", got)
	}
	if note := srv.currentDownloadsApplyStatus().Note; note != "Relabeled 1 of 1 download(s) already on seed storage." {
		t.Errorf("note = %q", note)
	}
}

// TestDownloads_RefusesWhileAnythingMoves: like the Plan page, the
// Downloads page shows no plan while qBittorrent is still moving
// something - and an apply can't start while another holds the lock.
func TestDownloads_RefusesWhileAnythingMoves(t *testing.T) {
	srv := newDownloadsTestServer(t)
	srv.qb.add(t, finishedDownload("aaa", "Linux.ISO", "manual", srv.ssd))
	srv.qb.add(t, fakeTorrent{Hash: "zzz", Name: "Elsewhere", Category: "other", SavePath: "/elsewhere", State: "moving", Progress: 1})

	page := httptest.NewRecorder()
	srv.handleDownloadsPage(page, httptest.NewRequest(http.MethodGet, "/downloads", nil))
	if !strings.Contains(page.Body.String(), "qBittorrent is still moving or checking 1 download(s) (Elsewhere)") || strings.Contains(page.Body.String(), "Move these downloads") {
		t.Fatalf("expected the plan refused while qBittorrent moves:\n%s", page.Body.String())
	}
	rec := serveForm(srv.handleDownloadsApplyStart, "/downloads/apply", nil)
	if !strings.Contains(rec.Body.String(), "refusing to apply") {
		t.Errorf("apply while qBittorrent moves:\n%s", rec.Body.String())
	}

	// The library Plan page refuses too: moves in one domain can be
	// filling the disk the other measures.
	plan := srv.buildPlanData()
	if !strings.Contains(plan.Error, "qBittorrent is still moving") {
		t.Errorf("library plan error = %q, want qBittorrent's move named", plan.Error)
	}
}

func TestDownloads_ScheduledRun(t *testing.T) {
	srv := newDownloadsTestServer(t)
	apprise := newFakeApprise(t)
	if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Notifications.Verbose = true
	srv.cfg.Scheduler.RunTorrentPlan = scheduler.Schedule{Enabled: true, Unit: scheduler.Hourly, Every: 1}

	// Nothing to move: a run that says so.
	now := time.Now()
	srv.tick(now)
	if got := apprise.notifications(); len(got) != 1 || !strings.Contains(got[0]["title"], "nothing to move") {
		t.Fatalf("notifications = %+v", got)
	}
	if srv.taskState(taskRunTorrentPlan).LastRan.IsZero() {
		t.Error("a run with nothing to move still counts as run")
	}

	srv.qb.add(t, finishedDownload("aaa", "Linux.ISO", "manual", srv.ssd))
	srv.runScheduledDownloadsPlan(now.Add(2 * time.Hour))
	waitDownloadsIdle(t, srv.Server)
	got := waitForNotifications(t, apprise, 3)
	if !strings.Contains(got[1]["title"], "Scheduled downloads move finished") || !strings.Contains(got[1]["body"], "moved 1, failed 0") || !strings.Contains(got[2]["title"], "Moved Linux.ISO") {
		t.Errorf("notifications = %+v", got)
	}

	// Another apply holding the lock means skip this tick, not fail it.
	srv.qb.add(t, finishedDownload("bbb", "Other.ISO", "manual", srv.ssd))
	before := srv.taskState(taskRunTorrentPlan).LastRan
	srv.currentDownloadsRun.active.Store(true)
	srv.runScheduledDownloadsPlan(now.Add(4 * time.Hour))
	srv.currentDownloadsRun.active.Store(false)
	if after := srv.taskState(taskRunTorrentPlan).LastRan; !after.Equal(before) {
		t.Error("a skipped tick must not count as a run")
	}
	if got := srv.qb.torrent("bbb"); got.SavePath != srv.ssd {
		t.Error("nothing moves while another apply is in flight")
	}
}

// TestDownloads_ScheduledRunWithoutQbittorrent: the task fails loudly, once
// per slot, when download tiering isn't set up.
func TestDownloads_ScheduledRunWithoutQbittorrent(t *testing.T) {
	srv := newDownloadsTestServer(t)
	apprise := newFakeApprise(t)
	if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
		t.Fatal(err)
	}
	if err := srv.connStore.Delete("qbittorrent"); err != nil {
		t.Fatal(err)
	}
	srv.runScheduledDownloadsPlan(time.Now())
	if got := apprise.notifications(); len(got) != 1 || got[0]["type"] != "failure" || !strings.Contains(got[0]["body"], "needs qBittorrent connected") {
		t.Fatalf("notifications = %+v", got)
	}
	page := httptest.NewRecorder()
	srv.handleDownloadsPage(page, httptest.NewRequest(http.MethodGet, "/downloads", nil))
	if !strings.Contains(page.Body.String(), "Download tiering needs qBittorrent connected") {
		t.Errorf("the page should explain what's missing:\n%s", page.Body.String())
	}
}
