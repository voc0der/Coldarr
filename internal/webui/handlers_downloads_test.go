package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/scheduler"
	"github.com/vocoder/coldarr/internal/secrets"
	"github.com/vocoder/coldarr/internal/torrents"
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
	srv.qb.add(t, fakeTorrent{Hash: "zzz", Name: "Elsewhere", Category: "other", SavePath: filepath.Join(srv.dir, "elsewhere"), State: "moving", Progress: 1})

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

// doneSeedingServer has two downloads past a 1-day requirement, their
// trackers scanned, and one that isn't.
func doneSeedingServer(t *testing.T, mode config.RemovalMode) *downloadsTestServer {
	t.Helper()
	srv := newDownloadsTestServer(t)
	zero := 0
	srv.cfg.Torrents.Seeding = config.SeedingConfig{MinSeedDays: 1, GraceDays: &zero, Overrides: []config.TrackerOverride{{Host: "strict.example.org", MinSeedDays: 30}}}
	srv.cfg.Torrents.Removal = config.RemovalConfig{Mode: mode}
	srv.qb.add(t, finishedDownload("aaa", "Old.ISO", "seed", srv.seed))
	srv.qb.add(t, finishedDownload("bbb", "Other.ISO", "seed", srv.seed))
	srv.qb.add(t, finishedDownload("ccc", "Strict.ISO", "seed", srv.seed))
	srv.qb.trackers["aaa"] = []string{"tracker.example.org"}
	srv.qb.trackers["bbb"] = []string{"tracker.example.org"}
	srv.qb.trackers["ccc"] = []string{"strict.example.org"}
	if _, err := srv.scanTrackersNow(); err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestDownloads_RemovalModes(t *testing.T) {
	// Off: just a count.
	srv := doneSeedingServer(t, config.RemovalOff)
	page := httptest.NewRecorder()
	srv.handleDownloadsPage(page, httptest.NewRequest(http.MethodGet, "/downloads", nil))
	if !strings.Contains(page.Body.String(), "2 download(s) have met their seeding requirement. Removal is off") {
		t.Fatalf("off:\n%s", page.Body.String())
	}

	// Report: the list, and no button.
	srv = doneSeedingServer(t, config.RemovalReport)
	page = httptest.NewRecorder()
	srv.handleDownloadsPage(page, httptest.NewRequest(http.MethodGet, "/downloads", nil))
	body := page.Body.String()
	if !strings.Contains(body, "Report only: these would be removed") || !strings.Contains(body, "Old.ISO") || strings.Contains(body, "Remove these now") {
		t.Fatalf("report:\n%s", body)
	}
	if strings.Contains(body, "<td>Strict.ISO") {
		t.Error("a download short of its tracker's override is not done")
	}
	rec := serveForm(srv.handleDownloadsRemove, "/downloads/remove", nil)
	if !strings.Contains(rec.Body.String(), "removal isn&#39;t set to remove") {
		t.Errorf("a crafted remove in report mode must be refused:\n%s", rec.Body.String())
	}
	if got := srv.qb.torrent("aaa"); got.Hash == "" {
		t.Fatal("report mode removed something")
	}
}

func TestDownloads_RemoveNow(t *testing.T) {
	srv := doneSeedingServer(t, config.RemovalRemove)
	srv.cfg.Torrents.Removal.MaxPerRun = 1
	apprise := newFakeApprise(t)
	if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
		t.Fatal(err)
	}

	page := httptest.NewRecorder()
	srv.handleDownloadsPage(page, httptest.NewRequest(http.MethodGet, "/downloads", nil))
	if !strings.Contains(page.Body.String(), "Remove these now") || !strings.Contains(page.Body.String(), "1 more wait for later runs") {
		t.Fatalf("remove mode, capped at 1:\n%s", page.Body.String())
	}

	rec := serveForm(srv.handleDownloadsRemove, "/downloads/remove", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "Removed 1 download(s)") {
		t.Fatalf("remove now:\n%s", body)
	}
	var gone, left []string
	for _, h := range []string{"aaa", "bbb", "ccc"} {
		if srv.qb.torrent(h).Hash == "" {
			gone = append(gone, h)
		} else {
			left = append(left, h)
		}
	}
	if len(gone) != 1 || strings.Join(left, ",") == "aaa,bbb,ccc" {
		t.Fatalf("gone = %v, want exactly one removed under the cap", gone)
	}
	names := map[string]string{"aaa": "Old.ISO", "bbb": "Other.ISO"}
	if _, err := os.Stat(filepath.Join(srv.seed, names[gone[0]])); !os.IsNotExist(err) {
		t.Error("its files should be deleted")
	}
	got := apprise.notifications()
	if len(got) != 1 || !strings.Contains(got[0]["body"], "removed 1 download(s)") || !strings.Contains(got[0]["body"], "1 more wait for the next run") {
		t.Errorf("notifications = %+v, want a summary even though verbose is off", got)
	}
	if !strings.Contains(body, "removed") || !strings.Contains(body, "Recent activity") {
		t.Errorf("the removal should be in the activity log:\n%s", body)
	}

	// Never alongside an apply.
	srv.currentDownloadsRun = &downloadsRun{done: make(chan struct{}), progress: mover.Run[torrents.MoveEntry](&torrents.Executor{}, nil, nil, mover.Options{})}
	srv.currentDownloadsRun.active.Store(true)
	rec = serveForm(srv.handleDownloadsRemove, "/downloads/remove", nil)
	srv.currentDownloadsRun.active.Store(false)
	if !strings.Contains(rec.Body.String(), "an apply is running") {
		t.Errorf("remove during an apply:\n%s", rec.Body.String())
	}
}

func TestScheduledExpireSeeds(t *testing.T) {
	for _, tt := range []struct {
		mode      config.RemovalMode
		wantTitle string
		wantBody  string
		removed   bool
	}{
		{mode: config.RemovalOff, wantTitle: "removal is off"},
		{mode: config.RemovalReport, wantTitle: "Finished seeds (report only)", wantBody: "2 download(s) would be removed"},
		{mode: config.RemovalRemove, wantTitle: "Scheduled removal of finished seeds", wantBody: "removed 2 download(s)", removed: true},
	} {
		t.Run(string(tt.mode), func(t *testing.T) {
			srv := doneSeedingServer(t, tt.mode)
			apprise := newFakeApprise(t)
			if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			srv.runScheduledExpireSeeds(now)
			got := apprise.notifications()
			if len(got) != 1 || !strings.Contains(got[0]["title"], tt.wantTitle) || !strings.Contains(got[0]["body"], tt.wantBody) {
				t.Fatalf("notifications = %+v", got)
			}
			if gone := srv.qb.torrent("aaa").Hash == ""; gone != tt.removed {
				t.Errorf("aaa removed = %v, want %v", gone, tt.removed)
			}
			if !srv.taskState(taskExpireSeeds).LastRan.Equal(now) {
				t.Error("the run should count")
			}
		})
	}
}

// TestDownloads_RemovalGuards: each guard in front of removal refuses
// before anything is removed - from the page, and from the scheduled task,
// which skips the tick (no notification, the run not counted) for what
// clears up on its own, and fails, notified and counted, for what doesn't.
func TestDownloads_RemovalGuards(t *testing.T) {
	for _, tt := range []struct {
		name    string
		setup   func(t *testing.T, srv *downloadsTestServer)
		want    string
		skipped bool
	}{
		{
			name: "qBittorrent still moving something",
			setup: func(t *testing.T, srv *downloadsTestServer) {
				srv.qb.add(t, fakeTorrent{Hash: "zzz", Name: "Elsewhere", Category: "other", SavePath: filepath.Join(srv.dir, "elsewhere"), State: "moving", Progress: 1})
			},
			want:    "refusing to remove anything: qBittorrent is still moving",
			skipped: true,
		},
		{
			name: "qBittorrent unreachable",
			setup: func(t *testing.T, srv *downloadsTestServer) {
				if err := srv.connStore.Set("qbittorrent", secrets.Connection{URL: "http://127.0.0.1:1"}); err != nil {
					t.Fatal(err)
				}
			},
			want:    "checking qBittorrent for in-flight moves",
			skipped: true,
		},
		{
			name: "another run holds the lock",
			setup: func(t *testing.T, srv *downloadsTestServer) {
				lock, err := mover.AcquireLock(srv.dir)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Release() })
			},
			want:    "another apply is running",
			skipped: true,
		},
		{
			name: "a drive failing its check",
			setup: func(t *testing.T, srv *downloadsTestServer) {
				srv.cfg.Tiers[0].Paths = []string{filepath.Join(srv.dir, "unplugged")}
			},
			want: "storage unavailable",
		},
		{
			name: "qBittorrent no longer connected",
			setup: func(t *testing.T, srv *downloadsTestServer) {
				if err := srv.connStore.Delete("qbittorrent"); err != nil {
					t.Fatal(err)
				}
			},
			want: "download tiering needs qBittorrent connected",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := doneSeedingServer(t, config.RemovalRemove)
			apprise := newFakeApprise(t)
			if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, srv)
			untouched := func(when string) {
				t.Helper()
				for hash, name := range map[string]string{"aaa": "Old.ISO", "bbb": "Other.ISO"} {
					if srv.qb.torrent(hash).Hash == "" {
						t.Fatalf("%s: %s was removed", when, hash)
					}
					if _, err := os.Stat(filepath.Join(srv.seed, name)); err != nil {
						t.Fatalf("%s: %s's files: %v", when, hash, err)
					}
				}
			}

			rec := serveForm(srv.handleDownloadsRemove, "/downloads/remove", nil)
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("page = %s, want %q", rec.Body.String(), tt.want)
			}
			untouched("from the page")

			now := time.Now()
			srv.runScheduledExpireSeeds(now)
			untouched("scheduled")
			got := apprise.notifications()
			counted := srv.taskState(taskExpireSeeds).LastRan.Equal(now)
			if tt.skipped {
				if len(got) != 0 || counted {
					t.Errorf("want the tick skipped: notifications = %+v, counted = %v", got, counted)
				}
				return
			}
			if len(got) != 1 || got[0]["type"] != "failure" || !strings.Contains(got[0]["body"], tt.want) || !counted {
				t.Errorf("want one failure naming %q, counted: notifications = %+v, counted = %v", tt.want, got, counted)
			}
		})
	}
}

// TestDownloads_RemovalThatDoesntTake: a delete qBittorrent accepts but
// doesn't carry out is a failure - on the page, and in the scheduled run's
// notification - and never recorded as removed.
func TestDownloads_RemovalThatDoesntTake(t *testing.T) {
	srv := doneSeedingServer(t, config.RemovalRemove)
	srv.qb.mu.Lock()
	srv.qb.dropDeletes = true
	srv.qb.mu.Unlock()

	rec := serveForm(srv.handleDownloadsRemove, "/downloads/remove", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "Removed 0 download(s)") || !strings.Contains(body, "2 couldn&#39;t be removed") || !strings.Contains(body, "qBittorrent still lists") {
		t.Fatalf("page:\n%s", body)
	}
	if strings.Contains(body, ">removed</span>") {
		t.Error("a removal that didn't happen was recorded")
	}

	apprise := newFakeApprise(t)
	if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Notifications.Verbose = true
	srv.runScheduledExpireSeeds(time.Now())
	got := apprise.notifications()
	if len(got) != 3 || got[0]["type"] != "warning" || !strings.Contains(got[0]["body"], "removed 0 download(s)") || !strings.Contains(got[0]["body"], "failed 2") || !strings.Contains(got[1]["title"], "Failed to remove a download") {
		t.Errorf("notifications = %+v, want a warning summary and, verbose, one per failure", got)
	}
}

// TestDownloads_ScheduledRunGuards: the scheduled move skips a tick while
// qBittorrent is moving something, and refuses - notified, counted - while
// a drive fails its check.
func TestDownloads_ScheduledRunGuards(t *testing.T) {
	srv := newDownloadsTestServer(t)
	apprise := newFakeApprise(t)
	if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
		t.Fatal(err)
	}
	srv.qb.add(t, finishedDownload("aaa", "Linux.ISO", "manual", srv.ssd))
	srv.qb.add(t, fakeTorrent{Hash: "zzz", Name: "Elsewhere", Category: "other", SavePath: filepath.Join(srv.dir, "elsewhere"), State: "moving", Progress: 1})

	now := time.Now()
	srv.runScheduledDownloadsPlan(now)
	if got := apprise.notifications(); len(got) != 0 || !srv.taskState(taskRunTorrentPlan).LastRan.IsZero() {
		t.Fatalf("want the tick skipped while qBittorrent moves: notifications = %+v", got)
	}

	srv.qb.mu.Lock()
	for _, tor := range srv.qb.torrents {
		tor.State = "stalledUP"
	}
	srv.qb.mu.Unlock()
	srv.cfg.Tiers[1].Paths = []string{filepath.Join(srv.dir, "unplugged")}
	srv.runScheduledDownloadsPlan(now)
	got := apprise.notifications()
	if len(got) != 1 || !strings.Contains(got[0]["title"], "Scheduled downloads move refused") || !strings.Contains(got[0]["body"], "storage unavailable") || !srv.taskState(taskRunTorrentPlan).LastRan.Equal(now) {
		t.Errorf("want one refusal, counted: notifications = %+v", got)
	}
	if srv.qb.torrent("aaa").SavePath != srv.ssd {
		t.Error("nothing moves while a drive fails its check")
	}
}

// TestDownloads_ApplyStartEdges: Move with nothing to move, or while a run
// is going, goes back to the page and starts nothing; with qBittorrent
// unreachable or not set up, the page says what's wrong.
func TestDownloads_ApplyStartEdges(t *testing.T) {
	srv := newDownloadsTestServer(t)
	if rec := serveForm(srv.handleDownloadsApplyStart, "/downloads/apply", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("nothing to move: %d, want back to the page", rec.Code)
	}

	srv.qb.add(t, finishedDownload("aaa", "Linux.ISO", "manual", srv.ssd))
	srv.currentDownloadsRun = &downloadsRun{done: make(chan struct{}), progress: mover.Run[torrents.MoveEntry](&torrents.Executor{}, nil, nil, mover.Options{})}
	srv.currentDownloadsRun.active.Store(true)
	rec := serveForm(srv.handleDownloadsApplyStart, "/downloads/apply", nil)
	srv.currentDownloadsRun = nil
	if rec.Code != http.StatusSeeOther || len(srv.qb.postedPaths()) != 0 {
		t.Errorf("a run in flight: %d, posts %v; want back to the page, nothing asked", rec.Code, srv.qb.postedPaths())
	}

	if err := srv.connStore.Set("qbittorrent", secrets.Connection{URL: "http://127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	if rec := serveForm(srv.handleDownloadsApplyStart, "/downloads/apply", nil); !strings.Contains(rec.Body.String(), "listing qBittorrent&#39;s downloads") {
		t.Errorf("qBittorrent unreachable:\n%s", rec.Body.String())
	}
	if err := srv.connStore.Delete("qbittorrent"); err != nil {
		t.Fatal(err)
	}
	if rec := serveForm(srv.handleDownloadsApplyStart, "/downloads/apply", nil); !strings.Contains(rec.Body.String(), "download tiering needs qBittorrent connected") {
		t.Errorf("qBittorrent not set up:\n%s", rec.Body.String())
	}
}
