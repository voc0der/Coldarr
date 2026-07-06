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

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/linkcache"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/scheduler"
	"github.com/vocoder/coldarr/internal/secrets"
)

// fakeApprise is a minimal Apprise API receiver that records every
// notification POSTed to it, safe for concurrent access.
type fakeApprise struct {
	*httptest.Server
	mu  sync.Mutex
	got []map[string]string
}

func newFakeApprise(t *testing.T) *fakeApprise {
	t.Helper()
	f := &fakeApprise{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.got = append(f.got, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeApprise) notifications() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]string, len(f.got))
	copy(out, f.got)
	return out
}

// fakeRadarr serves just enough of the Radarr v3 API for one movie to be
// planned and "moved." It never touches real files - mover's settle wait
// is configured short in these tests so it simply times out quickly
// regardless of whether any bytes actually land on the destination path.
type fakeRadarr struct {
	*httptest.Server
	mu          sync.Mutex
	editorCalls []map[string]any
}

func newFakeRadarr(t *testing.T, hotRoot string) *fakeRadarr {
	t.Helper()
	f := &fakeRadarr{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id": 1, "title": "Movie A", "titleSlug": "movie-a-2020",
			"path": hotRoot + "/Movie A", "rootFolderPath": hotRoot,
			"qualityProfileId": 1, "monitored": true, "hasFile": true,
			"added": "2020-01-01T00:00:00Z", "tags": []int{}, "sizeOnDisk": 5_000_000,
		}})
	})
	mux.HandleFunc("GET /api/v3/tag", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc("GET /api/v3/qualityprofile", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{}})
	})
	mux.HandleFunc("PUT /api/v3/movie/editor", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.editorCalls = append(f.editorCalls, body)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRadarr) calls() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.editorCalls))
	copy(out, f.editorCalls)
	return out
}

// testTierDirs creates the hot/cold directories a test's tiers point at,
// returning the parent temp dir too (for the history file and config
// path) - computed up front, before the fake Radarr server exists, since
// the fake server's movie needs to report these same real paths for the
// planner to recognize it as living on the hot tier.
func testTierDirs(t *testing.T) (dir, hotDir, coldDir string) {
	t.Helper()
	dir = t.TempDir()
	hotDir = filepath.Join(dir, "hot")
	coldDir = filepath.Join(dir, "cold")
	for _, d := range []string{hotDir, coldDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return dir, hotDir, coldDir
}

// newTestServer builds a *Server with one hot tier and one cold tier with
// room, wired to radarrURL and appriseURL. Policy is a zero-value
// PolicyConfig, which makes any non-protected, non-favorited, non-queued
// item eligible for Cold at score >= 0 - scoring correctness itself is
// covered by internal/scoring; this just needs some item guaranteed to
// plan a move.
func newTestServer(t *testing.T, dir, hotDir, coldDir, radarrURL, appriseURL string, verbose bool) *Server {
	t.Helper()

	cfg := &config.Config{
		Tiers: []model.Tier{
			{Name: "hot1", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie, model.TV}},
			{Name: "cold1", Role: model.RoleCold, Paths: []string{coldDir}, Media: []model.MediaType{model.Movie, model.TV}, MaxUsedPercent: 99, TargetUsedPercent: 95},
		},
		History:       config.HistoryConfig{Path: filepath.Join(dir, "history.json")},
		Notifications: config.NotificationsConfig{Verbose: verbose},
	}

	connStore, err := secrets.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("secrets.LoadOrCreate: %v", err)
	}
	if radarrURL != "" {
		if err := connStore.Set("radarr", secrets.Connection{URL: radarrURL, APIKey: "test", Enabled: true}); err != nil {
			t.Fatalf("connStore.Set radarr: %v", err)
		}
	}
	if appriseURL != "" {
		if err := connStore.Set("apprise", secrets.Connection{URL: appriseURL}); err != nil {
			t.Fatalf("connStore.Set apprise: %v", err)
		}
	}

	t.Setenv("COLDARR_SETTLE_CHECK_INTERVAL", "50ms")
	t.Setenv("COLDARR_SETTLE_STABLE_CHECKS", "1")
	t.Setenv("COLDARR_SETTLE_MAX_WAIT", "300ms")

	srv, err := New(filepath.Join(dir, "coldarr.yaml"), cfg, connStore)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestTick_RunScheduledPlan_MovesAndNotifiesSummary(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	apprise := newFakeApprise(t)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, apprise.URL, true)

	srv.cfg.Scheduler.RunPlan = scheduler.Schedule{Enabled: true, Unit: scheduler.Hourly, Every: 1}

	now := time.Now()
	srv.tick(now) // never-run hourly is due immediately

	deadline := time.Now().Add(3 * time.Second)
	for len(radarr.calls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	calls := radarr.calls()
	if len(calls) != 1 {
		t.Fatalf("radarr editor calls = %d, want 1", len(calls))
	}
	if got := calls[0]["rootFolderPath"]; got != srv.cfg.Tiers[1].Paths[0] {
		t.Errorf("editor call rootFolderPath = %v, want %s", got, srv.cfg.Tiers[1].Paths[0])
	}

	if got := srv.getLastRunPlan(); got.IsZero() || !got.Equal(now) {
		t.Errorf("lastRunPlan = %v, want %v", got, now)
	}
	if got := srv.getLastRanPlan(); got.IsZero() || !got.Equal(now) {
		t.Errorf("lastRanPlan = %v, want %v", got, now)
	}

	// Wait for the run to finish and send its summary (+ verbose items,
	// since Verbose=true above).
	deadline = time.Now().Add(3 * time.Second)
	for len(apprise.notifications()) < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	notifications := apprise.notifications()
	if len(notifications) == 0 {
		t.Fatal("no notifications received")
	}
	summary := notifications[0]
	if !strings.Contains(summary["title"], "Scheduled apply finished") {
		t.Errorf("summary title = %q, want to contain %q", summary["title"], "Scheduled apply finished")
	}
	if !strings.Contains(summary["body"], "moved 1, failed 0") {
		t.Errorf("summary body = %q, want to contain %q", summary["body"], "moved 1, failed 0")
	}

	if len(notifications) < 2 {
		t.Fatalf("verbose Item notification missing - got %d notifications, want >= 2", len(notifications))
	}
	item := notifications[1]
	if !strings.Contains(item["title"], "Movie A") {
		t.Errorf("item title = %q, want to contain %q", item["title"], "Movie A")
	}
}

func TestTick_RunScheduledPlan_SkipsWhenApplyAlreadyInFlight(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	apprise := newFakeApprise(t)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, apprise.URL, false)
	srv.cfg.Scheduler.RunPlan = scheduler.Schedule{Enabled: true, Unit: scheduler.Hourly, Every: 1}

	// Start a manual apply first - startApply returns as soon as the move
	// is queued; the underlying mover.Progress structurally cannot be
	// Done until its goroutine finishes a full runOne (HTTP call plus at
	// least one settle-check sleep), so checking immediately after is not
	// a timing race.
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
	if _, err := srv.startApply(eng, inv, plan); err != nil {
		t.Fatalf("startApply: %v", err)
	}

	if srv.currentRun.progress.Snapshot().Done {
		t.Fatal("test setup: apply finished before the scheduler tick could observe it in flight")
	}

	before := len(radarr.calls())
	srv.tick(now)

	if got := len(radarr.calls()); got != before {
		t.Errorf("radarr editor calls after tick = %d, want unchanged from %d - scheduled run should have skipped", got, before)
	}
	if got := srv.getLastRunPlan(); !got.IsZero() {
		t.Errorf("lastRunPlan = %v, want zero - a skipped tick must not update the due-check anchor", got)
	}
}

func TestTick_RunScheduledRescan_NotifiesColdUsage(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	apprise := newFakeApprise(t)
	srv := newTestServer(t, dir, hotDir, coldDir, "", apprise.URL, false)
	srv.cfg.Scheduler.RescanCold = scheduler.Schedule{Enabled: true, Unit: scheduler.Hourly, Every: 1}

	now := time.Now()
	srv.tick(now)

	deadline := time.Now().Add(3 * time.Second)
	for len(apprise.notifications()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	notifications := apprise.notifications()
	if len(notifications) != 1 {
		t.Fatalf("notifications = %d, want 1 (Verbose is off)", len(notifications))
	}
	if !strings.Contains(notifications[0]["title"], "Cold storage check") {
		t.Errorf("title = %q, want to contain %q", notifications[0]["title"], "Cold storage check")
	}
	if !strings.Contains(notifications[0]["body"], "cold1") {
		t.Errorf("body = %q, want to mention tier %q", notifications[0]["body"], "cold1")
	}

	if got := srv.getLastRanRescan(); got.IsZero() || !got.Equal(now) {
		t.Errorf("lastRanRescan = %v, want %v", got, now)
	}
}

func TestUpdateSchedule_ResetsAnchorNotLastRan(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	srv := newTestServer(t, dir, hotDir, coldDir, "", "", false)

	// Simulate a genuine prior run so lastRanPlan is non-zero.
	ranAt := time.Now().Add(-2 * time.Hour)
	srv.recordPlanRan(ranAt)

	if err := srv.updateSchedule("run_plan", scheduler.Schedule{Enabled: true, Unit: scheduler.Daily, Every: 1, At: "03:00"}); err != nil {
		t.Fatalf("updateSchedule: %v", err)
	}

	if got := srv.getLastRanPlan(); !got.Equal(ranAt) {
		t.Errorf("lastRanPlan = %v, want unchanged %v - saving a schedule must not claim a run that didn't happen", got, ranAt)
	}
	if got := srv.getLastRunPlan(); got.Equal(ranAt) || got.IsZero() {
		t.Errorf("lastRunPlan anchor = %v, want reset to ~now (not %v or zero)", got, ranAt)
	}
}

// TestTick_RunScheduledRefreshLinks_PopulatesCache confirms the scheduled
// "Refresh Links Cache" task actually fetches Radarr/Sonarr titleSlugs and
// Jellyfin's item-ID/server-ID and persists them, so Plan/History can read
// the link cache instead of hitting these apps live on every page view.
func TestTick_RunScheduledRefreshLinks_PopulatesCache(t *testing.T) {
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	apprise := newFakeApprise(t)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, apprise.URL, false)

	sonarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/series" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 7, "titleSlug": "some-show"}})
	}))
	t.Cleanup(sonarr.Close)

	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/System/Info":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "srv-123"})
		case "/Users":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "user1"}})
		case "/Users/user1/Items":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{
				{"Id": "jf-1", "Path": hotDir + "/Movie A"},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(jf.Close)

	if err := srv.connStore.Set("sonarr", secrets.Connection{URL: sonarr.URL, APIKey: "test", Enabled: true}); err != nil {
		t.Fatalf("connStore.Set sonarr: %v", err)
	}
	if err := srv.connStore.Set("jellyfin", secrets.Connection{URL: jf.URL, APIKey: "test", Enabled: true}); err != nil {
		t.Fatalf("connStore.Set jellyfin: %v", err)
	}

	srv.cfg.Scheduler.RefreshLinks = scheduler.Schedule{Enabled: true, Unit: scheduler.Hourly, Every: 1}

	now := time.Now()
	srv.tick(now)

	deadline := time.Now().Add(3 * time.Second)
	for srv.linkCache.Get().RefreshedAt.IsZero() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	snap := srv.linkCache.Get()
	if snap.RadarrTitleSlugByID[1] != "movie-a-2020" {
		t.Errorf("RadarrTitleSlugByID[1] = %q, want %q", snap.RadarrTitleSlugByID[1], "movie-a-2020")
	}
	if snap.SonarrTitleSlugByID[7] != "some-show" {
		t.Errorf("SonarrTitleSlugByID[7] = %q, want %q", snap.SonarrTitleSlugByID[7], "some-show")
	}
	if got := snap.JellyfinPathToID[filepath.Clean(hotDir+"/Movie A")]; got != "jf-1" {
		t.Errorf("JellyfinPathToID[%q] = %q, want %q", hotDir+"/Movie A", got, "jf-1")
	}
	if snap.JellyfinServerID != "srv-123" {
		t.Errorf("JellyfinServerID = %q, want %q", snap.JellyfinServerID, "srv-123")
	}

	if got := srv.getLastRanRefreshLinks(); got.IsZero() || !got.Equal(now) {
		t.Errorf("lastRanRefreshLinks = %v, want %v", got, now)
	}

	// The cache must also have been persisted to disk, not just held in
	// memory - a fresh Load from the same path should see it too.
	reloaded, err := linkcache.Load(linkCachePath(filepath.Join(dir, "coldarr.yaml")))
	if err != nil {
		t.Fatalf("linkcache.Load: %v", err)
	}
	if reloaded.Get().RefreshedAt.IsZero() {
		t.Errorf("reloaded link cache RefreshedAt is zero - Refresh did not persist to disk")
	}
}
