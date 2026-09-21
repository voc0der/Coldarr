package linkcache

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/vocoder/coldarr/internal/arrapi"
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
