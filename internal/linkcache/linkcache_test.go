package linkcache

import (
	"net/http"
	"net/http/httptest"
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
		_, _ = w.Write([]byte(`[{"id": 1, "titleSlug": "movie-a"}]`))
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
