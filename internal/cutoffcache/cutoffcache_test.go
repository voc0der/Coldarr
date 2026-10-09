package cutoffcache

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/arrapi"
)

func TestLoad_MissingFileStartsEmpty(t *testing.T) {
	s, err := Load(t.TempDir() + "/cutoffcache.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	snap := s.Get()
	if !snap.RefreshedAt.IsZero() {
		t.Error("a fresh store should report a zero RefreshedAt (never refreshed)")
	}
}

func TestGet_NilStoreIsEmpty(t *testing.T) {
	var s *Store
	snap := s.Get()
	if !snap.RefreshedAt.IsZero() || snap.RadarrUnmetIDs != nil || snap.SonarrUnmetIDs != nil {
		t.Errorf("expected a zero-value Snapshot from a nil *Store, got %+v", snap)
	}
}

func TestRefresh_SkipsNilClients(t *testing.T) {
	s, err := Load(t.TempDir() + "/cutoffcache.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Refresh(nil, nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := s.Get()
	if snap.RefreshedAt.IsZero() {
		t.Error("expected RefreshedAt to be set after a successful Refresh, even with nothing configured")
	}
	if snap.RadarrUnmetIDs != nil || snap.SonarrUnmetIDs != nil {
		t.Errorf("expected no data for unconfigured apps, got %+v", snap)
	}
}

func TestRefresh_PersistsAcrossLoad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"records": [{"id": 7}], "totalRecords": 1}`))
	}))
	defer srv.Close()

	path := t.TempDir() + "/cutoffcache.json"
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	radarr := arrapi.NewRadarrClient(srv.URL, "key")
	if err := s.Refresh(radarr, nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load (reloaded): %v", err)
	}
	snap := reloaded.Get()
	if !snap.RadarrUnmetIDs[7] {
		t.Fatalf("RadarrUnmetIDs[7] = %v, want true (snapshot: %+v)", snap.RadarrUnmetIDs[7], snap)
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

	s, err := Load(t.TempDir() + "/cutoffcache.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	radarr := arrapi.NewRadarrClient(srv.URL, "key")
	if err := s.Refresh(radarr, nil); err == nil {
		t.Fatal("expected Refresh to fail when the configured Radarr is unreachable")
	}
	if !s.Get().RefreshedAt.IsZero() {
		t.Error("a failed Refresh must not update the stored snapshot")
	}
}

func TestRefresh_SonarrSeriesIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/wanted/cutoff" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"records": [{"seriesId": 7}, {"seriesId": 7}, {"seriesId": 9}], "totalRecords": 3}`))
	}))
	defer srv.Close()

	s, err := Load(t.TempDir() + "/cutoffcache.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Refresh(nil, arrapi.NewSonarrClient(srv.URL, "key")); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := s.Get()
	if len(snap.SonarrUnmetIDs) != 2 || !snap.SonarrUnmetIDs[7] || !snap.SonarrUnmetIDs[9] {
		t.Fatalf("SonarrUnmetIDs = %v, want series 7 and 9 (one entry per series, not per episode)", snap.SonarrUnmetIDs)
	}
	if snap.RadarrUnmetIDs != nil {
		t.Errorf("RadarrUnmetIDs = %v, want nil with Radarr unconfigured", snap.RadarrUnmetIDs)
	}
}

func TestRefresh_SonarrFailureLeavesSnapshotUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s, err := Load(t.TempDir() + "/cutoffcache.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Refresh(nil, arrapi.NewSonarrClient(srv.URL, "key")); err == nil || !strings.Contains(err.Error(), "sonarr") {
		t.Fatalf("Refresh error = %v, want a sonarr fetch failure", err)
	}
	if !s.Get().RefreshedAt.IsZero() {
		t.Error("a failed Refresh must not update the stored snapshot")
	}
}

// TestLoad_EmptyOrUnreadableCache: an empty file is a cache that was never
// written, but a corrupt or unreadable one is an error - the server refuses
// to start on it rather than quietly forgetting which items to keep hot.
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
	if _, err := Load(corrupt); err == nil || !strings.Contains(err.Error(), "parsing quality-cutoff cache") {
		t.Errorf("Load(corrupt) error = %v, want a parse error", err)
	}

	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "reading quality-cutoff cache") {
		t.Errorf("Load(directory) error = %v, want a read error", err)
	}
}

func TestRefresh_ReportsAFailedSave(t *testing.T) {
	tests := []struct {
		name      string
		breakSave func(path string) error
		wantErr   string
	}{
		{name: "directory cannot be created", breakSave: func(path string) error { return os.WriteFile(filepath.Dir(path), nil, 0o600) }, wantErr: "creating quality-cutoff cache directory"},
		{name: "temp file cannot be written", breakSave: func(path string) error { return os.MkdirAll(path+".tmp", 0o750) }, wantErr: "writing"},
		{name: "file replaced by a directory", breakSave: func(path string) error { return os.MkdirAll(path, 0o750) }, wantErr: "saving"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", "cutoffcache.json")
			s, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if err := tt.breakSave(path); err != nil {
				t.Fatalf("breakSave: %v", err)
			}
			if err := s.Refresh(nil, nil); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Refresh error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
