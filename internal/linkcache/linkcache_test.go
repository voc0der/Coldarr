package linkcache

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/jellyfin"
)

func TestLoad_MissingFileStartsEmpty(t *testing.T) {
	s, err := Load(t.TempDir() + "/linkcache.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	snap := s.Get()
	if !snap.RefreshedAt.IsZero() {
		t.Error("a fresh store should report a zero RefreshedAt (never refreshed)")
	}
}

func TestRefresh_SkipsNilClients(t *testing.T) {
	s, err := Load(t.TempDir() + "/linkcache.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Refresh(nil, nil, nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := s.Get()
	if snap.RefreshedAt.IsZero() {
		t.Error("expected RefreshedAt to be set after a successful Refresh, even with nothing configured")
	}
	if snap.RadarrTitleSlugByID != nil || snap.SonarrTitleSlugByID != nil || snap.JellyfinPathToID != nil {
		t.Errorf("expected no data for unconfigured apps, got %+v", snap)
	}
}

func TestRefresh_PersistsAcrossLoad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id": 1, "titleSlug": "movie-a", "path": "/cold/Movie A"}]`))
	}))
	defer srv.Close()

	path := t.TempDir() + "/linkcache.json"
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	radarr := arrapi.NewRadarrClient(srv.URL, "key")
	if err := s.Refresh(radarr, nil, nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load (reloaded): %v", err)
	}
	snap := reloaded.Get()
	if snap.RadarrTitleSlugByID[1] != "movie-a" {
		t.Fatalf("RadarrTitleSlugByID[1] = %q, want movie-a (snapshot: %+v)", snap.RadarrTitleSlugByID[1], snap)
	}
	if snap.RadarrPathByID[1] != "/cold/Movie A" {
		t.Fatalf("RadarrPathByID[1] = %q, want /cold/Movie A (snapshot: %+v)", snap.RadarrPathByID[1], snap)
	}
	if snap.RefreshedAt.IsZero() {
		t.Error("expected RefreshedAt to survive a save/load roundtrip")
	}
}

func TestRefresh_FailurePropagatesAndLeavesSnapshotUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s, err := Load(t.TempDir() + "/linkcache.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	radarr := arrapi.NewRadarrClient(srv.URL, "key")
	if err := s.Refresh(radarr, nil, nil); err == nil {
		t.Fatal("expected Refresh to fail when the configured Radarr is unreachable")
	}
	if !s.Get().RefreshedAt.IsZero() {
		t.Error("a failed Refresh must not update the stored snapshot")
	}
}

// TestLoad_CacheFromBeforeItemFolders pins upgrade safety: a cache written
// before item folders were cached must still load (a failed load stops the
// server from starting), with its slugs intact and no folders until the
// next refresh.
func TestLoad_CacheFromBeforeItemFolders(t *testing.T) {
	path := t.TempDir() + "/linkcache.json"
	old := `{"radarr_title_slug_by_id": {"1": "movie-a"}, "sonarr_title_slug_by_id": {"7": "show-a"}, "jellyfin_path_to_id": {"/cold/Movie A": "jf-1"}, "jellyfin_server_id": "srv-1", "refreshed_at": "2026-01-02T03:04:05Z"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	snap := s.Get()
	if snap.RadarrTitleSlugByID[1] != "movie-a" || snap.SonarrTitleSlugByID[7] != "show-a" || snap.JellyfinPathToID["/cold/Movie A"] != "jf-1" {
		t.Fatalf("old cache lost data on load: %+v", snap)
	}
	if snap.RadarrPathByID != nil || snap.SonarrPathByID != nil {
		t.Fatalf("old cache should have no item folders yet: %+v", snap)
	}
}

// linkServers stands up a Sonarr with one series and a single-user Jellyfin
// whose library holds that series' folder. failing names the one endpoint
// that answers 500 instead ("" for none).
func linkServers(t *testing.T, failing string) (sonarr *arrapi.SonarrClient, jf *jellyfin.Client) {
	t.Helper()
	sonarrSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`[{"id": 7, "titleSlug": "show-a", "path": "/cold/Show A"}]`))
	}))
	t.Cleanup(sonarrSrv.Close)

	jfSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			_, _ = w.Write([]byte(`{"Items": [{"Id": "jf-7", "Type": "Series", "Path": "/cold/Show A"}]}`))
		case "/System/Info":
			_, _ = w.Write([]byte(`{"Id": "server-1"}`))
		default:
			t.Errorf("unexpected jellyfin path %s", r.URL.Path)
		}
	}))
	t.Cleanup(jfSrv.Close)

	return arrapi.NewSonarrClient(sonarrSrv.URL, "key"), jellyfin.NewClient(jfSrv.URL, "key")
}

func TestRefresh_SonarrAndJellyfin(t *testing.T) {
	s, err := Load(t.TempDir() + "/linkcache.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sonarr, jf := linkServers(t, "")
	if err := s.Refresh(nil, sonarr, jf); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := s.Get()
	if snap.SonarrTitleSlugByID[7] != "show-a" || snap.SonarrPathByID[7] != "/cold/Show A" {
		t.Errorf("sonarr link targets = (%v, %v), want series 7's slug and folder", snap.SonarrTitleSlugByID, snap.SonarrPathByID)
	}
	if snap.JellyfinPathToID["/cold/Show A"] != "jf-7" {
		t.Errorf("JellyfinPathToID = %v, want /cold/Show A -> jf-7", snap.JellyfinPathToID)
	}
	if snap.JellyfinServerID != "server-1" {
		t.Errorf("JellyfinServerID = %q, want server-1", snap.JellyfinServerID)
	}
	if snap.RadarrTitleSlugByID != nil {
		t.Errorf("RadarrTitleSlugByID = %v, want nil with Radarr unconfigured", snap.RadarrTitleSlugByID)
	}
}

// TestRefresh_AnyFailedLookupLeavesSnapshotUnchanged: each lookup a
// configured app answers is all-or-nothing - half a refresh would mix a
// fresh Sonarr catalog with a stale Jellyfin one.
func TestRefresh_AnyFailedLookupLeavesSnapshotUnchanged(t *testing.T) {
	tests := []struct {
		failing string
		wantErr string
	}{
		{failing: "/api/v3/series", wantErr: "fetching sonarr link targets"},
		{failing: "/Users/u1/Items", wantErr: "fetching jellyfin library item IDs"},
		{failing: "/System/Info", wantErr: "fetching jellyfin server ID"},
	}
	for _, tt := range tests {
		t.Run(tt.failing, func(t *testing.T) {
			s, err := Load(t.TempDir() + "/linkcache.json")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			sonarr, jf := linkServers(t, tt.failing)
			if err := s.Refresh(nil, sonarr, jf); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Refresh error = %v, want one containing %q", err, tt.wantErr)
			}
			if !s.Get().RefreshedAt.IsZero() {
				t.Error("a failed Refresh must not update the stored snapshot")
			}
		})
	}
}

// TestLoad_EmptyOrUnreadableCache: an empty file is a cache that was never
// written, but a corrupt or unreadable one is an error.
func TestLoad_EmptyOrUnreadableCache(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(empty)
	if err != nil {
		t.Fatalf("Load(empty file): %v", err)
	}
	if !s.Get().RefreshedAt.IsZero() {
		t.Error("an empty cache file should load as never refreshed")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(corrupt); err == nil || !strings.Contains(err.Error(), "parsing link cache") {
		t.Errorf("Load(corrupt) error = %v, want a parse error", err)
	}

	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "reading link cache") {
		t.Errorf("Load(directory) error = %v, want a read error", err)
	}
}

func TestRefresh_ReportsAFailedSave(t *testing.T) {
	tests := []struct {
		name      string
		breakSave func(path string) error
		wantErr   string
	}{
		{name: "directory cannot be created", breakSave: func(path string) error { return os.WriteFile(filepath.Dir(path), nil, 0o600) }, wantErr: "creating link cache directory"},
		{name: "temp file cannot be written", breakSave: func(path string) error { return os.MkdirAll(path+".tmp", 0o750) }, wantErr: "writing"},
		{name: "file replaced by a directory", breakSave: func(path string) error { return os.MkdirAll(path, 0o750) }, wantErr: "saving"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", "linkcache.json")
			s, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := tt.breakSave(path); err != nil {
				t.Fatalf("breakSave: %v", err)
			}
			if err := s.Refresh(nil, nil, nil); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Refresh error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
