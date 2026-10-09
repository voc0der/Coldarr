package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/cutoffcache"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/planner"
	"github.com/vocoder/coldarr/internal/secrets"
)

func testTierDirs(t *testing.T) (hotDir string) {
	t.Helper()
	dir := t.TempDir()
	hotDir = filepath.Join(dir, "hot")
	if err := os.MkdirAll(hotDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return hotDir
}

func testHistory(t *testing.T) *history.Store {
	t.Helper()
	h, err := history.Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}
	return h
}

func radarrServer(t *testing.T, moviePath string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/movie":
			_, _ = w.Write([]byte(`[{"id": 1, "title": "Movie A", "path": "` + moviePath + `", "rootFolderPath": "/hot", "status": "released"}]`))
		case "/api/v3/tag", "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"records": []}`))
		default:
			t.Errorf("unexpected radarr path %s", r.URL.Path)
		}
	}))
}

func TestBuildInventory_PopulatesPathStatusAndItems(t *testing.T) {
	hotDir := testTierDirs(t)
	srv := radarrServer(t, filepath.Join(hotDir, "Movie A"))
	defer srv.Close()

	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie}},
		}},
		Radarr:  arrapi.NewRadarrClient(srv.URL, "key"),
		History: testHistory(t),
	}

	inv, err := e.BuildInventory(time.Now())
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}

	status, ok := inv.PathStatus[hotDir]
	if !ok || status.Err != nil {
		t.Fatalf("expected %s to be a usable path, got %+v", hotDir, status)
	}
	if len(inv.Items) != 1 || inv.Items[0].Item.Title != "Movie A" {
		t.Fatalf("expected 1 item titled Movie A, got %+v", inv.Items)
	}
}

func TestBuildInventory_MissingPathIsErrNotFatal(t *testing.T) {
	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{"/does/not/exist"}},
		}},
		History: testHistory(t),
	}

	inv, err := e.BuildInventory(time.Now())
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	if inv.PathStatus["/does/not/exist"].Err == nil {
		t.Fatal("expected a missing tier path to report an error on its PathStatus, not fail the whole build")
	}
}

func TestBuildInventory_RadarrFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := &Engine{
		Cfg:     &config.Config{Tiers: []model.Tier{{Name: "hot", Role: model.RoleHot, Paths: []string{testTierDirs(t)}}}},
		Radarr:  arrapi.NewRadarrClient(srv.URL, "key"),
		History: testHistory(t),
	}

	if _, err := e.BuildInventory(time.Now()); err == nil {
		t.Fatal("expected an unreachable Radarr to fail BuildInventory")
	}
}

func TestBuildInventory_JellyfinFavoriteMarksItem(t *testing.T) {
	hotDir := testTierDirs(t)
	moviePath := filepath.Join(hotDir, "Movie A")
	radarr := radarrServer(t, moviePath)
	defer radarr.Close()

	// Jellyfin reports a Movie's Path as the video file itself, one level
	// inside the folder Radarr manages - not the folder. If Coldarr ever
	// goes back to matching Jellyfin Path against Radarr's (folder) Path
	// verbatim, this file-vs-folder mismatch must make the favorite match
	// fail, catching the regression this test exists to prevent.
	movieFilePath := filepath.Join(moviePath, "Movie A.mkv")
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "j1", "Path": "` + movieFilePath + `", "Type": "Movie"}]}`))
		default:
			t.Errorf("unexpected jellyfin path %s", r.URL.Path)
		}
	}))
	defer jf.Close()

	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie}},
		}},
		Radarr:       arrapi.NewRadarrClient(radarr.URL, "key"),
		History:      testHistory(t),
		jellyfinConn: secrets.Connection{URL: jf.URL, APIKey: "key", Enabled: true},
		jellyfinOK:   true,
	}

	inv, err := e.BuildInventory(time.Now())
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	if len(inv.Items) != 1 || !inv.Items[0].Item.JellyfinFavorite {
		t.Fatalf("expected the item to be marked as a Jellyfin favorite, got %+v", inv.Items)
	}
}

func TestBuildInventory_JellyfinFailureFailsClosed(t *testing.T) {
	hotDir := testTierDirs(t)
	radarr := radarrServer(t, filepath.Join(hotDir, "Movie A"))
	defer radarr.Close()

	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer jf.Close()

	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie}},
		}},
		Radarr:       arrapi.NewRadarrClient(radarr.URL, "key"),
		History:      testHistory(t),
		jellyfinConn: secrets.Connection{URL: jf.URL, APIKey: "key", Enabled: true},
		jellyfinOK:   true,
	}

	inv, err := e.BuildInventory(time.Now())
	if err == nil {
		t.Fatal("expected an unavailable configured Jellyfin to fail BuildInventory")
	}
	if inv != nil {
		t.Fatalf("expected no usable inventory when favorite protection could not be established, got %+v", inv)
	}
	if !strings.Contains(err.Error(), "refusing to continue because favorite protection could not be established") {
		t.Fatalf("expected a fail-closed explanation, got %v", err)
	}
}

// TestBuildInventory_NeverHitsWantedCutoffLive guards against the v0.18.0
// regression this feature shipped with: FetchMovies must never call
// Radarr's /wanted/cutoff itself - that endpoint is slow enough on real
// libraries that doing so on every Dashboard/Plan page load turned an
// upgrade into an outage. QualityCutoffNotMet is only ever set from
// internal/cutoffcache below.
func TestBuildInventory_NeverHitsWantedCutoffLive(t *testing.T) {
	hotDir := testTierDirs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/movie":
			_, _ = w.Write([]byte(`[{"id": 1, "title": "Movie A", "path": "` + filepath.Join(hotDir, "Movie A") + `", "rootFolderPath": "/hot", "status": "released"}]`))
		case "/api/v3/tag", "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"records": []}`))
		case "/api/v3/wanted/cutoff":
			t.Error("BuildInventory must never call /wanted/cutoff live - it's too slow on real libraries; this belongs to internal/cutoffcache's own background refresh only")
		default:
			t.Errorf("unexpected radarr path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cache, err := cutoffcache.Load(t.TempDir() + "/cutoffcache.json")
	if err != nil {
		t.Fatalf("cutoffcache.Load: %v", err)
	}

	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie}},
		}},
		Radarr:      arrapi.NewRadarrClient(srv.URL, "key"),
		History:     testHistory(t),
		CutoffCache: cache,
	}

	if _, err := e.BuildInventory(time.Now()); err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
}

// TestBuildInventory_AnnotatesQualityCutoffFromCache confirms the engine
// layer (not arrapi) is what sets MediaItem.QualityCutoffNotMet, reading
// whatever internal/cutoffcache already has cached - never fetching it
// live.
func TestBuildInventory_AnnotatesQualityCutoffFromCache(t *testing.T) {
	hotDir := testTierDirs(t)
	moviePath := filepath.Join(hotDir, "Movie A")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/movie":
			_, _ = w.Write([]byte(`[{"id": 1, "title": "Movie A", "path": "` + moviePath + `", "rootFolderPath": "/hot", "status": "released"}]`))
		case "/api/v3/tag", "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"records": []}`))
		case "/api/v3/wanted/cutoff":
			_, _ = w.Write([]byte(`{"records": [{"id": 1}], "totalRecords": 1}`))
		default:
			t.Errorf("unexpected radarr path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	radarr := arrapi.NewRadarrClient(srv.URL, "key")
	cache, err := cutoffcache.Load(t.TempDir() + "/cutoffcache.json")
	if err != nil {
		t.Fatalf("cutoffcache.Load: %v", err)
	}
	// Seed the cache via a real Refresh (the only way it's ever
	// populated in production, from the "Scan Quality Cutoffs" scheduled
	// task or its manual trigger) - not by hand-constructing a Snapshot,
	// so this test exercises the actual production path.
	if err := cache.Refresh(radarr, nil); err != nil {
		t.Fatalf("cache.Refresh: %v", err)
	}

	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie}},
		}},
		Radarr:      radarr,
		History:     testHistory(t),
		CutoffCache: cache,
	}

	inv, err := e.BuildInventory(time.Now())
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	if len(inv.Items) != 1 || !inv.Items[0].Item.QualityCutoffNotMet {
		t.Fatalf("expected movie 1 to be annotated QualityCutoffNotMet=true from the cache, got %+v", inv.Items)
	}
	if len(inv.Warnings) != 0 {
		t.Fatalf("expected no warnings once the cache has been refreshed, got %v", inv.Warnings)
	}
}

// TestBuildInventory_WarnsWhenCutoffCacheNeverRefreshed confirms an
// operator gets told (via the Dashboard's warning banner) that
// quality-cutoff protection is inactive until they enable or manually
// run "Scan Quality Cutoffs" - rather than it silently doing nothing.
func TestBuildInventory_WarnsWhenCutoffCacheNeverRefreshed(t *testing.T) {
	hotDir := testTierDirs(t)
	srv := radarrServer(t, filepath.Join(hotDir, "Movie A"))
	defer srv.Close()

	cache, err := cutoffcache.Load(t.TempDir() + "/cutoffcache.json")
	if err != nil {
		t.Fatalf("cutoffcache.Load: %v", err)
	}

	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie}},
		}},
		Radarr:      arrapi.NewRadarrClient(srv.URL, "key"),
		History:     testHistory(t),
		CutoffCache: cache,
	}

	inv, err := e.BuildInventory(time.Now())
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	if inv.Items[0].Item.QualityCutoffNotMet {
		t.Error("expected QualityCutoffNotMet=false when the cache has never been refreshed")
	}
	if len(inv.Warnings) == 0 {
		t.Fatal("expected a warning that quality-cutoff scanning has never run")
	}
}

// TestBuildInventory_NilCutoffCacheIsSafe confirms a nil CutoffCache (as
// in every other test in this file, which construct &Engine{} directly
// without going through New) behaves like an empty, never-refreshed
// cache rather than panicking.
func TestBuildInventory_NilCutoffCacheIsSafe(t *testing.T) {
	hotDir := testTierDirs(t)
	srv := radarrServer(t, filepath.Join(hotDir, "Movie A"))
	defer srv.Close()

	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie}},
		}},
		Radarr:  arrapi.NewRadarrClient(srv.URL, "key"),
		History: testHistory(t),
	}

	inv, err := e.BuildInventory(time.Now())
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	if inv.Items[0].Item.QualityCutoffNotMet {
		t.Error("expected QualityCutoffNotMet=false with a nil CutoffCache")
	}
}

func TestInventory_SharedVolumePaths(t *testing.T) {
	inv := &Inventory{PathStatus: map[string]PathStatus{
		"/a": {DeviceID: 1, DeviceIDKnown: true},
		"/b": {DeviceID: 1, DeviceIDKnown: true},
		"/c": {DeviceID: 2, DeviceIDKnown: true},
	}}
	siblings := inv.SharedVolumePaths("/a")
	if len(siblings) != 1 || siblings[0] != "/b" {
		t.Fatalf("expected [/b], got %v", siblings)
	}
	if len(inv.SharedVolumePaths("/c")) != 0 {
		t.Fatal("expected /c (unique device) to have no siblings")
	}
}

func TestJellyfinClient_NilWhenNotConfigured(t *testing.T) {
	e := &Engine{}
	if e.JellyfinClient() != nil {
		t.Fatal("expected a nil JellyfinClient when Jellyfin isn't configured")
	}
}

func TestJellyfinClient_ConfiguredReturnsClient(t *testing.T) {
	e := &Engine{jellyfinOK: true, jellyfinConn: secrets.Connection{URL: "http://example.invalid", APIKey: "key"}}
	if e.JellyfinClient() == nil {
		t.Fatal("expected a non-nil JellyfinClient when Jellyfin is configured")
	}
}

func TestEnvDuration(t *testing.T) {
	const name = "COLDARR_TEST_SETTLE_INTERVAL"

	if got := envDuration(name); got != 0 {
		t.Errorf("unset env var: got %v, want 0", got)
	}

	t.Setenv(name, "5s")
	if got := envDuration(name); got != 5*time.Second {
		t.Errorf("got %v, want 5s", got)
	}

	t.Setenv(name, "not-a-duration")
	if got := envDuration(name); got != 0 {
		t.Errorf("invalid duration: got %v, want 0", got)
	}
}

func TestEnvInt(t *testing.T) {
	const name = "COLDARR_TEST_SETTLE_CHECKS"

	if got := envInt(name); got != 0 {
		t.Errorf("unset env var: got %v, want 0", got)
	}

	t.Setenv(name, "3")
	if got := envInt(name); got != 3 {
		t.Errorf("got %v, want 3", got)
	}

	t.Setenv(name, "not-a-number")
	if got := envInt(name); got != 0 {
		t.Errorf("invalid int: got %v, want 0", got)
	}
}

// TestNotifyJellyfinMoved_ReportsOnlyWhatTheRunCouldNot is the join
// between the mover's mid-run reporting and the end-of-run refresh.
// Reporting a path Jellyfin already has queued restarts its library
// monitor's debounce, pushing back the very rescan that earlier report
// asked for - so an item the run already reported has to be resolved and
// refreshed here without being reported a second time.
func TestNotifyJellyfinMoved_ReportsOnlyWhatTheRunCouldNot(t *testing.T) {
	// Bounds the test against the real multi-minute budget if resolution
	// ever regresses, rather than hanging the suite.
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_TIMEOUT", "5s")

	var mu sync.Mutex
	var reported, refreshed []string
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/Library/Media/Updated":
			var body struct {
				Updates []struct {
					Path string `json:"Path"`
				} `json:"Updates"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			for _, u := range body.Updates {
				reported = append(reported, u.Path)
			}
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items":
			// A Movie reports its video file, a Series its folder - so this
			// also covers itemFolderPath normalizing both onto the folder
			// Radarr/Sonarr named.
			_, _ = w.Write([]byte(`{"Items": [
				{"Id": "id-reported", "Type": "Movie", "Path": "/cold/movies/Reported (2024)/Reported (2024).mkv"},
				{"Id": "id-unreported", "Type": "Series", "Path": "/cold/tv/Unreported"}
			]}`))
		case strings.HasSuffix(r.URL.Path, "/Refresh"):
			mu.Lock()
			refreshed = append(refreshed, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer jf.Close()

	e := &Engine{
		jellyfinConn: secrets.Connection{URL: jf.URL, APIKey: "key", Enabled: true},
		jellyfinOK:   true,
	}

	err := e.NotifyJellyfinMoved([]mover.EntryProgress{
		{
			Entry:      planner.MoveEntry{Item: model.MediaItem{Title: "Reported", Path: "/hot/movies/Reported (2024)"}},
			LandedPath: "/cold/movies/Reported (2024)",
			Reported:   true,
		},
		{
			Entry:      planner.MoveEntry{Item: model.MediaItem{Title: "Unreported", Path: "/hot/tv/Unreported"}},
			LandedPath: "/cold/tv/Unreported",
			Reported:   false,
		},
	}, nil)
	if err != nil {
		t.Fatalf("NotifyJellyfinMoved: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Only the item the run couldn't report, and both halves of it.
	wantReported := []string{"/cold/tv/Unreported", "/hot/tv/Unreported"}
	slices.Sort(reported)
	if !slices.Equal(reported, wantReported) {
		t.Errorf("reported paths = %v, want %v", reported, wantReported)
	}

	// Both items still get the full refresh - that half is never skipped.
	wantRefreshed := []string{"/Items/id-reported/Refresh", "/Items/id-unreported/Refresh"}
	slices.Sort(refreshed)
	if !slices.Equal(refreshed, wantRefreshed) {
		t.Errorf("refreshed = %v, want %v", refreshed, wantRefreshed)
	}
}

func TestStartJellyfinFollowUp_NilWithoutJellyfin(t *testing.T) {
	plan := &planner.Plan{Entries: []planner.MoveEntry{{Item: model.MediaItem{Title: "Movie A", Path: "/hot/movies/Movie A"}}}}
	followUp, err := (&Engine{}).StartJellyfinFollowUp(plan)
	if err != nil || followUp != nil {
		t.Fatalf("StartJellyfinFollowUp() = (%v, %v), want (nil, nil) with Jellyfin unconfigured", followUp, err)
	}
}

// TestJellyfinFollowUp_PutsBackDateAsTheMoveLands pins when the date goes
// back: on the mover's landing signal for that item, not at the end of the
// run, so a run that stops part way through has only lost the items still
// waiting on Jellyfin. The end of the run then waits for that follow-up
// and does not repeat it.
func TestJellyfinFollowUp_PutsBackDateAsTheMoveLands(t *testing.T) {
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_TIMEOUT", "5s")
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_INTERVAL", "10ms")

	var mu sync.Mutex
	landed := false
	refreshes := 0
	var sent string
	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/Library/Media/Updated":
			landed = true
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items" && !landed:
			_, _ = w.Write([]byte(`{"Items": [{"Id": "id-old", "Type": "Movie", "Path": "/hot/movies/Moved (2021)/Moved (2021).mkv", "DateCreated": "2021-03-14T12:00:00.0000000Z"}]}`))
		case r.URL.Path == "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "id-new", "Type": "Movie", "Path": "/cold/movies/Moved (2021)/Moved (2021).mkv", "DateCreated": "2026-09-30T02:15:00.0000000Z"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/Users/u1/Items/id-new":
			_, _ = w.Write([]byte(`{"Id": "id-new", "Name": "Moved", "DateCreated": "2026-09-30T02:15:00.0000000Z"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/Items/id-new":
			var body struct {
				DateCreated string `json:"DateCreated"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sent = body.DateCreated
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/Items/id-new/Refresh":
			refreshes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer jf.Close()

	e := &Engine{
		jellyfinConn: secrets.Connection{URL: jf.URL, APIKey: "key", Enabled: true},
		jellyfinOK:   true,
	}
	entry := planner.MoveEntry{Item: model.MediaItem{Title: "Moved", Path: "/hot/movies/Moved (2021)"}}
	followUp, err := e.StartJellyfinFollowUp(&planner.Plan{Entries: []planner.MoveEntry{entry}})
	if err != nil {
		t.Fatalf("StartJellyfinFollowUp: %v", err)
	}

	// The mover's landing signal, with the rest of the run still to go.
	if err := followUp.ReportMoved("/hot/movies/Moved (2021)", "/cold/movies/Moved (2021)"); err != nil {
		t.Fatalf("ReportMoved: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		done := refreshes == 1
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the landed item was never followed up before the end of the run")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if sent != "2021-03-14T12:00:00.0000000Z" {
		t.Errorf("date added sent = %q, want the one recorded before the move", sent)
	}
	mu.Unlock()

	err = e.NotifyJellyfinMoved([]mover.EntryProgress{{
		Entry: entry, LandedPath: "/cold/movies/Moved (2021)", Reported: true,
	}}, followUp)
	if err != nil {
		t.Fatalf("NotifyJellyfinMoved: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshes != 1 {
		t.Errorf("refreshes = %d, want 1: the end of the run must not repeat a landed item's follow-up", refreshes)
	}
}

// testStore returns an encrypted connection store in dir holding conns.
func testStore(t *testing.T, dir string, conns map[string]secrets.Connection) *secrets.Store {
	t.Helper()
	store, err := secrets.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("secrets.LoadOrCreate: %v", err)
	}
	for app, conn := range conns {
		if err := store.Set(app, conn); err != nil {
			t.Fatalf("store.Set %s: %v", app, err)
		}
	}
	return store
}

func TestNew_WiresConfiguredConnections(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, dir, map[string]secrets.Connection{
		"radarr":   {URL: "http://radarr:7878", APIKey: "r"},
		"sonarr":   {URL: "http://sonarr:8989", APIKey: "s"},
		"jellyfin": {URL: "http://jellyfin:8096", APIKey: "j", Enabled: true},
	})

	// The quality-cutoff cache lives next to the history file, not at a
	// path of its own - seed one there to prove New reads that location.
	cutoffs := `{"radarr_unmet_ids": {"5": true}, "refreshed_at": "2026-01-02T03:04:05Z"}`
	if err := os.WriteFile(filepath.Join(dir, "coldarr-cutoffcache.json"), []byte(cutoffs), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	e, err := New(&config.Config{History: config.HistoryConfig{Path: filepath.Join(dir, "history.json")}}, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.Radarr == nil || e.Sonarr == nil {
		t.Fatalf("expected both Arr clients, got Radarr=%v Sonarr=%v", e.Radarr, e.Sonarr)
	}
	if e.JellyfinClient() == nil {
		t.Fatal("expected an enabled Jellyfin connection to produce a client")
	}
	if e.History == nil {
		t.Fatal("expected a history store")
	}
	if !e.CutoffCache.Get().RadarrUnmetIDs[5] {
		t.Fatalf("cutoff cache = %+v, want the one seeded beside history.json", e.CutoffCache.Get())
	}
}

// TestNew_NothingConfiguredIsNotAnError: the web GUI builds an Engine on a
// fresh install to render the empty dashboard, so a missing app - or a
// Jellyfin connection saved but switched off - must not fail construction.
func TestNew_NothingConfiguredIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, dir, map[string]secrets.Connection{
		"jellyfin": {URL: "http://jellyfin:8096", APIKey: "j", Enabled: false},
	})

	e, err := New(&config.Config{History: config.HistoryConfig{Path: filepath.Join(dir, "history.json")}}, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.Radarr != nil || e.Sonarr != nil {
		t.Fatalf("expected no Arr clients, got Radarr=%v Sonarr=%v", e.Radarr, e.Sonarr)
	}
	if e.JellyfinClient() != nil {
		t.Fatal("a disabled Jellyfin connection must not produce a client")
	}
}

func TestNew_FailsOnCorruptState(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		wantErr string
	}{
		{name: "history", file: "history.json", wantErr: "parsing history"},
		{name: "cutoff cache", file: "coldarr-cutoffcache.json", wantErr: "parsing quality-cutoff cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tt.file), []byte("{not json"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			_, err := New(&config.Config{History: config.HistoryConfig{Path: filepath.Join(dir, "history.json")}}, testStore(t, dir, nil))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("New error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestCheckStorage_OneDeadPathBlocksEverything pins the dead-drive rule:
// any tier path failing its check refuses every move, not just moves to or
// from that drive, and the error names the tier so the operator knows
// which drive to go and look at.
func TestCheckStorage_OneDeadPathBlocksEverything(t *testing.T) {
	hot := testTierDirs(t)
	e := &Engine{Cfg: &config.Config{Tiers: []model.Tier{
		{Name: "hot", Role: model.RoleHot, Paths: []string{hot}},
		{Name: "sat1", Role: model.RoleCold, Paths: []string{filepath.Join(t.TempDir(), "unplugged")}},
	}}}

	err := e.CheckStorage()
	if err == nil {
		t.Fatal("expected a missing tier path to refuse storage")
	}
	if !strings.Contains(err.Error(), "sat1:") || !strings.Contains(err.Error(), "refusing to move anything") {
		t.Fatalf("CheckStorage error = %v, want it to name tier sat1 and refuse", err)
	}

	e.Cfg.Tiers = e.Cfg.Tiers[:1]
	if err := e.CheckStorage(); err != nil {
		t.Fatalf("CheckStorage with every path healthy = %v, want nil", err)
	}
}

// commandServer answers Radarr/Sonarr's GET /api/v3/command with body, or
// a 500 when body is empty.
func commandServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/command" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if body == "" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestArrMovesInFlight(t *testing.T) {
	const idle = `[{"id": 1, "name": "MoveMovie", "status": "completed"}]`
	const twoMoves = `[{"id": 1, "name": "MoveMovie", "status": "started"}, {"id": 2, "name": "MoveMovie", "status": "queued"}]`
	const oneMove = `[{"id": 3, "name": "MoveSeries", "status": "started"}]`

	t.Run("nothing configured is idle", func(t *testing.T) {
		busy, detail, err := (&Engine{}).ArrMovesInFlight()
		if busy || detail != "" || err != nil {
			t.Fatalf("ArrMovesInFlight() = (%v, %q, %v), want idle", busy, detail, err)
		}
	})

	t.Run("idle apps", func(t *testing.T) {
		e := &Engine{
			Radarr: arrapi.NewRadarrClient(commandServer(t, idle).URL, "key"),
			Sonarr: arrapi.NewSonarrClient(commandServer(t, idle).URL, "key"),
		}
		busy, detail, err := e.ArrMovesInFlight()
		if busy || detail != "" || err != nil {
			t.Fatalf("ArrMovesInFlight() = (%v, %q, %v), want idle", busy, detail, err)
		}
	})

	t.Run("moves in both apps are each reported", func(t *testing.T) {
		e := &Engine{
			Radarr: arrapi.NewRadarrClient(commandServer(t, twoMoves).URL, "key"),
			Sonarr: arrapi.NewSonarrClient(commandServer(t, oneMove).URL, "key"),
		}
		busy, detail, err := e.ArrMovesInFlight()
		if err != nil || !busy {
			t.Fatalf("ArrMovesInFlight() = (%v, %q, %v), want busy", busy, detail, err)
		}
		for _, want := range []string{"Radarr is still executing 2 move command(s)", "Sonarr is still executing 1 move command(s)", "across Coldarr restarts"} {
			if !strings.Contains(detail, want) {
				t.Errorf("detail = %q, want it to contain %q", detail, want)
			}
		}
	})

	// "Can't tell" must fail toward not moving anything: an error from
	// either app is returned as an error, never read as idle.
	for _, app := range []string{"radarr", "sonarr"} {
		t.Run(app+" error is an error, not idle", func(t *testing.T) {
			radarrBody, sonarrBody := idle, idle
			if app == "radarr" {
				radarrBody = ""
			} else {
				sonarrBody = ""
			}
			e := &Engine{
				Radarr: arrapi.NewRadarrClient(commandServer(t, radarrBody).URL, "key"),
				Sonarr: arrapi.NewSonarrClient(commandServer(t, sonarrBody).URL, "key"),
			}
			busy, _, err := e.ArrMovesInFlight()
			if err == nil || !strings.Contains(err.Error(), "checking "+app) {
				t.Fatalf("ArrMovesInFlight() = (%v, %v), want an error naming %s", busy, err, app)
			}
		})
	}
}

func TestInventory_PathViews(t *testing.T) {
	hot := model.Tier{Name: "hot", Role: model.RoleHot}
	cold := model.Tier{Name: "cold", Role: model.RoleCold}
	inv := &Inventory{PathStatus: map[string]PathStatus{
		"/hot":      {Tier: hot, Path: "/hot", Usage: diskusage.Usage{UsedPercent: 50}, DeviceID: 1, DeviceIDKnown: true},
		"/cold1":    {Tier: cold, Path: "/cold1", Usage: diskusage.Usage{UsedPercent: 70}, DeviceID: 2, DeviceIDKnown: true},
		"/cold2":    {Tier: cold, Path: "/cold2", Err: os.ErrNotExist},
		"/unknowns": {Tier: hot, Path: "/unknowns", Usage: diskusage.Usage{UsedPercent: 10}},
	}}

	usable := inv.UsableUsage()
	if _, ok := usable["/cold2"]; ok {
		t.Error("a path that failed its checks must be absent from usable usage, never assumed empty")
	}
	if len(usable) != 3 || usable["/cold1"].UsedPercent != 70 {
		t.Errorf("UsableUsage() = %+v, want the three healthy paths", usable)
	}

	if tier, ok := inv.TierOf("/cold1"); !ok || tier.Name != "cold" {
		t.Errorf("TierOf(/cold1) = (%+v, %v), want cold", tier, ok)
	}
	if _, ok := inv.TierOf("/elsewhere"); ok {
		t.Error("TierOf an unconfigured path should report false")
	}

	volumes := inv.VolumeOf()
	if len(volumes) != 2 || volumes["/hot"] != 1 || volumes["/cold1"] != 2 {
		t.Errorf("VolumeOf() = %v, want only the two paths with a known device", volumes)
	}

	var coldPaths []string
	for _, status := range inv.ColdTierPaths() {
		coldPaths = append(coldPaths, status.Path)
	}
	slices.Sort(coldPaths)
	if !slices.Equal(coldPaths, []string{"/cold1", "/cold2"}) {
		t.Errorf("ColdTierPaths() = %v, want both cold paths, failed or not", coldPaths)
	}

	if got := inv.SharedVolumePaths("/unknowns"); got != nil {
		t.Errorf("SharedVolumePaths of a path with no known device = %v, want nil", got)
	}
	if got := inv.SharedVolumePaths("/elsewhere"); got != nil {
		t.Errorf("SharedVolumePaths of an unconfigured path = %v, want nil", got)
	}
}

func sonarrServer(t *testing.T, seriesPath string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/series":
			_, _ = w.Write([]byte(`[{"id": 7, "title": "Show A", "path": "` + seriesPath + `", "rootFolderPath": "/hot", "status": "ended", "statistics": {"sizeOnDisk": 100, "episodeFileCount": 3}}]`))
		case "/api/v3/tag", "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"records": []}`))
		case "/api/v3/wanted/cutoff":
			_, _ = w.Write([]byte(`{"records": [{"seriesId": 7}], "totalRecords": 1}`))
		default:
			t.Errorf("unexpected sonarr path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBuildInventory_SonarrSeriesAnnotatedFromCutoffCache(t *testing.T) {
	hotDir := testTierDirs(t)
	sonarr := arrapi.NewSonarrClient(sonarrServer(t, filepath.Join(hotDir, "Show A")).URL, "key")

	cache, err := cutoffcache.Load(t.TempDir() + "/cutoffcache.json")
	if err != nil {
		t.Fatalf("cutoffcache.Load: %v", err)
	}
	if err := cache.Refresh(nil, sonarr); err != nil {
		t.Fatalf("cache.Refresh: %v", err)
	}

	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.TV}},
		}},
		Sonarr:      sonarr,
		History:     testHistory(t),
		CutoffCache: cache,
	}

	inv, err := e.BuildInventory(time.Now())
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	if len(inv.Items) != 1 || inv.Items[0].Item.ArrApp != "sonarr" || !inv.Items[0].Item.QualityCutoffNotMet {
		t.Fatalf("items = %+v, want series 7 annotated QualityCutoffNotMet from the cache", inv.Items)
	}
}

func TestBuildInventory_SonarrFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := &Engine{
		Cfg:     &config.Config{Tiers: []model.Tier{{Name: "hot", Role: model.RoleHot, Paths: []string{testTierDirs(t)}}}},
		Sonarr:  arrapi.NewSonarrClient(srv.URL, "key"),
		History: testHistory(t),
	}

	_, err := e.BuildInventory(time.Now())
	if err == nil || !strings.Contains(err.Error(), "fetching series from sonarr") {
		t.Fatalf("BuildInventory error = %v, want a sonarr fetch failure", err)
	}
}

// TestBuildPlan_MovesAnEligibleItemToColdStorage is the end-to-end join
// of the two read-only halves the CLI and GUI both run before any apply:
// a real inventory of real directories, planned against.
func TestBuildPlan_MovesAnEligibleItemToColdStorage(t *testing.T) {
	root := t.TempDir()
	hotDir, coldDir := filepath.Join(root, "hot"), filepath.Join(root, "cold")
	for _, d := range []string{hotDir, coldDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	radarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/movie":
			_, _ = w.Write([]byte(`[{"id": 1, "title": "Movie A", "path": "` + filepath.Join(hotDir, "Movie A") + `", "rootFolderPath": "` + hotDir + `", "hasFile": true, "monitored": true, "added": "2020-01-01T00:00:00Z", "sizeOnDisk": 5000000, "status": "released"}]`))
		case "/api/v3/tag", "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"records": []}`))
		default:
			t.Errorf("unexpected radarr path %s", r.URL.Path)
		}
	}))
	defer radarr.Close()

	e := &Engine{
		Cfg: &config.Config{Tiers: []model.Tier{
			{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie}},
			{Name: "cold", Role: model.RoleCold, Paths: []string{coldDir}, Media: []model.MediaType{model.Movie}, MaxUsedPercent: 99, TargetUsedPercent: 95},
		}},
		Radarr:  arrapi.NewRadarrClient(radarr.URL, "key"),
		History: testHistory(t),
	}

	now := time.Now()
	inv, err := e.BuildInventory(now)
	if err != nil {
		t.Fatalf("BuildInventory: %v", err)
	}
	plan, err := e.BuildPlan(inv, now)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("plan entries = %+v, want Movie A planned", plan.Entries)
	}
	if entry := plan.Entries[0]; entry.Item.Title != "Movie A" || entry.ToPath != coldDir || entry.ToTier != "cold" {
		t.Fatalf("plan entry = %+v, want Movie A moved to %s", entry, coldDir)
	}
}

// TestMovers_AppliesSettleOverridesAndFollowUp pins Movers' wiring: the
// settle-timing env overrides, the storage re-check before each move, and
// the typed-nil guard - a nil *JellyfinFollowUp stored in the interface
// field would make Reporter non-nil and panic on first use.
func TestMovers_AppliesSettleOverridesAndFollowUp(t *testing.T) {
	t.Setenv("COLDARR_SETTLE_CHECK_INTERVAL", "2s")
	t.Setenv("COLDARR_SETTLE_STABLE_CHECKS", "4")
	t.Setenv("COLDARR_SETTLE_MAX_WAIT", "90m")

	e := &Engine{Cfg: &config.Config{}, History: testHistory(t)}
	m := e.Movers(nil)
	if m.SettleCheckInterval != 2*time.Second || m.SettleStableChecks != 4 || m.SettleMaxWait != 90*time.Minute {
		t.Errorf("settle timing = (%v, %d, %v), want the env overrides (2s, 4, 90m)", m.SettleCheckInterval, m.SettleStableChecks, m.SettleMaxWait)
	}
	if m.History != e.History || m.CheckStorage == nil {
		t.Error("Movers must carry the engine's history and its storage check")
	}
	if m.Reporter != nil {
		t.Fatalf("Reporter = %#v with no follow-up, want a nil interface", m.Reporter)
	}

	followUp := &JellyfinFollowUp{}
	if got := e.Movers(followUp).Reporter; got != mover.MoveReporter(followUp) {
		t.Fatalf("Reporter = %#v, want the follow-up passed in", got)
	}
}

// fakeJellyfin records the paths reported to it and the library scans and
// item refreshes it is asked for. Until an item is listed at its new path
// (see list), it never resolves.
type fakeJellyfin struct {
	*httptest.Server
	mu            sync.Mutex
	failReports   bool
	failScan      bool
	list          string
	reported      []string
	scans         int
	itemRefreshes int
}

func newFakeJellyfin(t *testing.T) *fakeJellyfin {
	t.Helper()
	f := &fakeJellyfin{list: `{"Items": []}`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/Library/Media/Updated":
			if f.failReports {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			var body struct {
				Updates []struct {
					Path string `json:"Path"`
				} `json:"Updates"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, u := range body.Updates {
				f.reported = append(f.reported, u.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/Library/Refresh":
			f.scans++
			if f.failScan {
				http.Error(w, "scan refused", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case r.URL.Path == "/Users/u1/Items":
			_, _ = w.Write([]byte(f.list))
		case strings.HasSuffix(r.URL.Path, "/Refresh"):
			f.itemRefreshes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected jellyfin %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeJellyfin) engine() *Engine {
	return &Engine{jellyfinConn: secrets.Connection{URL: f.URL, APIKey: "key", Enabled: true}, jellyfinOK: true}
}

func TestNotifyJellyfinMoved_NoopWithoutJellyfin(t *testing.T) {
	moved := []mover.EntryProgress{{Entry: planner.MoveEntry{Item: model.MediaItem{Title: "Movie A"}}, LandedPath: "/cold/Movie A"}}
	if err := (&Engine{}).NotifyJellyfinMoved(moved, nil); err != nil {
		t.Fatalf("NotifyJellyfinMoved without Jellyfin = %v, want nil", err)
	}
}

// TestNotifyJellyfinMoved_UnlandedItemFallsBackToLibraryScan: a move with
// no recorded landing path can't be targeted by path, so the only help left
// is the whole-library scan - and the error still says which item that was.
func TestNotifyJellyfinMoved_UnlandedItemFallsBackToLibraryScan(t *testing.T) {
	jf := newFakeJellyfin(t)
	moved := []mover.EntryProgress{{Entry: planner.MoveEntry{Item: model.MediaItem{Title: "Lost Movie", Path: "/hot/Lost Movie"}}}}

	err := jf.engine().NotifyJellyfinMoved(moved, nil)
	if err == nil {
		t.Fatal("expected an error naming the item that could not be targeted")
	}
	for _, want := range []string{"no landed path recorded for Lost Movie", "fell back to a whole-library scan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	if jf.scans != 1 {
		t.Errorf("library scans = %d, want exactly 1 fallback scan", jf.scans)
	}
}

func TestNotifyJellyfinMoved_ReportsAFailedFallbackScan(t *testing.T) {
	jf := newFakeJellyfin(t)
	jf.failScan = true
	moved := []mover.EntryProgress{{Entry: planner.MoveEntry{Item: model.MediaItem{Title: "Lost Movie"}}}}

	err := jf.engine().NotifyJellyfinMoved(moved, nil)
	if err == nil || !strings.Contains(err.Error(), "whole-library fallback scan also failed") {
		t.Fatalf("NotifyJellyfinMoved error = %v, want the fallback scan's failure reported too", err)
	}
}

// TestNotifyJellyfinMoved_FailedFollowUpIsSurfacedNotRepeated: an item
// whose mid-run follow-up never resolved is reported as a failure at the
// end of the run (with the library-scan fallback), but is neither reported
// to Jellyfin nor resolved a second time.
func TestNotifyJellyfinMoved_FailedFollowUpIsSurfacedNotRepeated(t *testing.T) {
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_TIMEOUT", "50ms")
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_INTERVAL", "5ms")
	jf := newFakeJellyfin(t)
	e := jf.engine()

	followUp, err := e.StartJellyfinFollowUp(&planner.Plan{})
	if err != nil || followUp == nil {
		t.Fatalf("StartJellyfinFollowUp(empty plan) = (%v, %v), want a follow-up with nothing snapshotted", followUp, err)
	}
	if err := followUp.ReportMoved("/hot/Movie A", "/cold/Movie A"); err != nil {
		t.Fatalf("ReportMoved: %v", err)
	}

	err = e.NotifyJellyfinMoved([]mover.EntryProgress{{
		Entry:      planner.MoveEntry{Item: model.MediaItem{Title: "Movie A", Path: "/hot/Movie A"}},
		LandedPath: "/cold/Movie A",
		Reported:   true,
	}}, followUp)
	if err == nil {
		t.Fatal("expected the follow-up's failure to be reported")
	}
	for _, want := range []string{"no Jellyfin item appeared at /cold/Movie A", "fell back to a whole-library scan"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}

	jf.mu.Lock()
	defer jf.mu.Unlock()
	if want := []string{"/hot/Movie A", "/cold/Movie A"}; !slices.Equal(jf.reported, want) {
		t.Errorf("reported paths = %v, want only the mid-run report %v", jf.reported, want)
	}
}

// TestJellyfinFollowUp_FailedReportIsLeftForTheEndOfTheRun: a report that
// fails as the move lands comes back as an error (so the mover marks the
// item unreported) and starts no follow-up - leaving NotifyJellyfinMoved to
// report and refresh it once the run is over.
func TestJellyfinFollowUp_FailedReportIsLeftForTheEndOfTheRun(t *testing.T) {
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_TIMEOUT", "5s")
	t.Setenv("COLDARR_JELLYFIN_RESOLVE_INTERVAL", "5ms")
	jf := newFakeJellyfin(t)
	jf.failReports = true
	e := jf.engine()

	followUp, err := e.StartJellyfinFollowUp(&planner.Plan{})
	if err != nil {
		t.Fatalf("StartJellyfinFollowUp: %v", err)
	}
	if err := followUp.ReportMoved("/hot/Movie A", "/cold/Movie A"); err == nil {
		t.Fatal("expected a failed report to be returned to the mover")
	}
	if outcome := followUp.wait(); len(outcome) != 0 {
		t.Fatalf("follow-up outcomes = %v, want none started for a failed report", outcome)
	}

	jf.mu.Lock()
	jf.failReports = false
	jf.list = `{"Items": [{"Id": "id-a", "Type": "Movie", "Path": "/cold/Movie A/Movie A.mkv"}]}`
	jf.mu.Unlock()

	err = e.NotifyJellyfinMoved([]mover.EntryProgress{{
		Entry:      planner.MoveEntry{Item: model.MediaItem{Title: "Movie A", Path: "/hot/Movie A"}},
		LandedPath: "/cold/Movie A",
		Reported:   false,
	}}, followUp)
	if err != nil {
		t.Fatalf("NotifyJellyfinMoved: %v", err)
	}

	jf.mu.Lock()
	defer jf.mu.Unlock()
	if want := []string{"/hot/Movie A", "/cold/Movie A"}; !slices.Equal(jf.reported, want) {
		t.Errorf("reported paths = %v, want the end of the run to report %v", jf.reported, want)
	}
	if jf.itemRefreshes != 1 || jf.scans != 0 {
		t.Errorf("item refreshes = %d, library scans = %d, want the item refreshed directly with no fallback scan", jf.itemRefreshes, jf.scans)
	}
}
