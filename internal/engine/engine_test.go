package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
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
		case "/api/v3/wanted/cutoff":
			_, _ = w.Write([]byte(`{"records": [], "totalRecords": 0}`))
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

	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "j1", "Path": "` + moviePath + `"}]}`))
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

func TestBuildInventory_JellyfinFailureIsWarningNotError(t *testing.T) {
	hotDir := testTierDirs(t)
	radarr := radarrServer(t, filepath.Join(hotDir, "Movie A"))
	defer radarr.Close()

	jf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
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
		t.Fatalf("expected a Jellyfin failure to degrade to a warning, not fail BuildInventory: %v", err)
	}
	if len(inv.Warnings) == 0 {
		t.Fatal("expected a warning about the unreachable Jellyfin")
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
