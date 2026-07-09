package orphans

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/jellyfin"
	"github.com/vocoder/coldarr/internal/model"
)

func TestLoad_MissingFileStartsEmpty(t *testing.T) {
	s, err := Load(t.TempDir() + "/orphans.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	snap := s.Get()
	if !snap.ScannedAt.IsZero() {
		t.Error("a fresh store should report a zero ScannedAt (never scanned)")
	}
}

func TestGet_NilStoreIsEmpty(t *testing.T) {
	var s *Store
	snap := s.Get()
	if !snap.ScannedAt.IsZero() || snap.Candidates != nil {
		t.Errorf("expected a zero-value Snapshot from a nil *Store, got %+v", snap)
	}
}

func TestRefresh_SkipsNilClients(t *testing.T) {
	s, err := Load(t.TempDir() + "/orphans.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s.Refresh(nil, nil, nil, nil); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := s.Get()
	if snap.ScannedAt.IsZero() {
		t.Error("expected ScannedAt to be set after a successful Refresh, even with nothing configured")
	}
	if snap.Candidates != nil {
		t.Errorf("expected no candidates with no tiers configured, got %+v", snap.Candidates)
	}
}

func TestRefresh_FailurePropagatesAndLeavesSnapshotUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s, err := Load(t.TempDir() + "/orphans.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	radarr := arrapi.NewRadarrClient(srv.URL, "key")
	if err := s.Refresh(radarr, nil, nil, nil); err == nil {
		t.Fatal("expected Refresh to fail when the configured Radarr is unreachable")
	}
	if !s.Get().ScannedAt.IsZero() {
		t.Error("a failed Refresh must not update the stored snapshot")
	}
}

func TestRefresh_SkipsUnavailableTierPath(t *testing.T) {
	s, err := Load(t.TempDir() + "/orphans.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tiers := []model.Tier{
		{Name: "cold", Role: model.RoleCold, Paths: []string{t.TempDir() + "/does-not-exist"}, Media: []model.MediaType{model.Movie}},
	}
	if err := s.Refresh(nil, nil, nil, tiers); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := s.Get()
	if len(snap.Warnings) == 0 {
		t.Error("expected a warning for the unavailable tier path")
	}
	if len(snap.Candidates) != 0 {
		t.Errorf("expected no candidates from an unavailable path, got %+v", snap.Candidates)
	}
}

func TestRefresh_PersistsAcrossLoad(t *testing.T) {
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "Orphaned Movie"))
	mustWriteFile(t, filepath.Join(root, "Orphaned Movie", "movie.mkv"), 42)

	path := t.TempDir() + "/orphans.json"
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tiers := []model.Tier{
		{Name: "cold", Role: model.RoleCold, Paths: []string{root}, Media: []model.MediaType{model.Movie}},
	}
	if err := s.Refresh(nil, nil, nil, tiers); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load (reloaded): %v", err)
	}
	snap := reloaded.Get()
	if snap.ScannedAt.IsZero() {
		t.Error("expected ScannedAt to survive a save/load roundtrip")
	}
	if len(snap.Candidates) != 1 || snap.Candidates[0].SizeBytes != 42 {
		t.Fatalf("expected 1 candidate of size 42 to survive a roundtrip, got %+v", snap.Candidates)
	}
}

// TestRefresh_PrunesKnownItemsAndFindsOrphans is the core scan behavior:
// a known item's own folder (from Radarr, Sonarr, or Jellyfin - Jellyfin
// items count as known even if Radarr/Sonarr never heard of them) is
// never descended into or flagged, an unrelated folder next to it is
// reported exactly once at its own root (not per file inside it), and
// organizational folders above a known item are still walked through to
// find orphans elsewhere under them.
func TestRefresh_PrunesKnownItemsAndFindsOrphans(t *testing.T) {
	root := t.TempDir()

	knownMoviePath := filepath.Join(root, "Movies", "Known Movie")
	mustMkdir(t, knownMoviePath)
	mustWriteFile(t, filepath.Join(knownMoviePath, "movie.mkv"), 100)

	orphanMoviePath := filepath.Join(root, "Movies", "Orphaned Movie")
	mustMkdir(t, orphanMoviePath)
	mustWriteFile(t, filepath.Join(orphanMoviePath, "movie.mkv"), 55)

	knownShowPath := filepath.Join(root, "TV", "Known Show")
	mustMkdir(t, filepath.Join(knownShowPath, "Season 01"))
	mustWriteFile(t, filepath.Join(knownShowPath, "Season 01", "ep1.mkv"), 10)
	mustMkdir(t, filepath.Join(knownShowPath, "Season 02"))
	mustWriteFile(t, filepath.Join(knownShowPath, "Season 02", "ep1.mkv"), 10)

	orphanShowPath := filepath.Join(root, "TV", "Orphaned Show")
	mustMkdir(t, filepath.Join(orphanShowPath, "Season 01"))
	mustWriteFile(t, filepath.Join(orphanShowPath, "Season 01", "ep1.mkv"), 77)

	// Known only to Jellyfin - must not be treated as an orphan just
	// because Radarr/Sonarr never heard of it.
	homeVideoPath := filepath.Join(root, "HomeVideos", "Vacation 2020")
	mustMkdir(t, homeVideoPath)
	mustWriteFile(t, filepath.Join(homeVideoPath, "video.mp4"), 5)

	radarrSrv := fakeRadarrServer(t, map[int]string{1: knownMoviePath})
	defer radarrSrv.Close()
	sonarrSrv := fakeSonarrServer(t, map[int]string{1: knownShowPath})
	defer sonarrSrv.Close()
	jellyfinSrv := fakeJellyfinServer(t, []string{homeVideoPath})
	defer jellyfinSrv.Close()

	s, err := Load(t.TempDir() + "/orphans.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tiers := []model.Tier{
		{Name: "cold", Role: model.RoleCold, Paths: []string{root}, Media: []model.MediaType{model.Movie, model.TV}},
	}
	radarr := arrapi.NewRadarrClient(radarrSrv.URL, "key")
	sonarr := arrapi.NewSonarrClient(sonarrSrv.URL, "key")
	jf := jellyfin.NewClient(jellyfinSrv.URL, "key")

	if err := s.Refresh(radarr, sonarr, jf, tiers); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := s.Get()
	if len(snap.Candidates) != 2 {
		t.Fatalf("expected exactly 2 orphan candidates, got %d: %+v", len(snap.Candidates), snap.Candidates)
	}

	byPath := map[string]Candidate{}
	for _, c := range snap.Candidates {
		byPath[c.Path] = c
	}

	orphanMovie, ok := byPath[orphanMoviePath]
	if !ok {
		t.Fatalf("expected %q reported as an orphan, got %+v", orphanMoviePath, snap.Candidates)
	}
	if orphanMovie.SizeBytes != 55 {
		t.Errorf("orphan movie size = %d, want 55", orphanMovie.SizeBytes)
	}

	orphanShow, ok := byPath[orphanShowPath]
	if !ok {
		t.Fatalf("expected %q reported as an orphan, got %+v", orphanShowPath, snap.Candidates)
	}
	if orphanShow.SizeBytes != 77 {
		t.Errorf("orphan show size = %d, want 77 (just its own file, not the known show's)", orphanShow.SizeBytes)
	}

	if _, flagged := byPath[knownMoviePath]; flagged {
		t.Errorf("known movie folder %q must never be flagged as an orphan", knownMoviePath)
	}
	if _, flagged := byPath[filepath.Join(knownShowPath, "Season 02")]; flagged {
		t.Errorf("a known show's own internal season folder must never be flagged as an orphan")
	}
	if _, flagged := byPath[homeVideoPath]; flagged {
		t.Errorf("a Jellyfin-only known item must not be flagged as an orphan just because Radarr/Sonarr don't know it")
	}
}

func TestRefresh_ProbesWritability(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root - permission bits don't restrict writes")
	}

	writableDir := t.TempDir()

	readOnlyParent := t.TempDir()
	readOnlyDir := filepath.Join(readOnlyParent, "ro")
	mustMkdir(t, readOnlyDir)
	if err := os.Chmod(readOnlyDir, 0o500); err != nil { //nolint:gosec // directory perms (need +x to stat/list), simulating a read-only mount for this test only
		t.Fatalf("Chmod: %v", err)
	}
	defer func() { _ = os.Chmod(readOnlyDir, 0o700) }() //nolint:gosec // restoring so t.TempDir() cleanup can remove it

	s, err := Load(t.TempDir() + "/orphans.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tiers := []model.Tier{
		{Name: "writable", Role: model.RoleCold, Paths: []string{writableDir}, Media: []model.MediaType{model.Movie}},
		{Name: "read-only", Role: model.RoleCold, Paths: []string{readOnlyDir}, Media: []model.MediaType{model.Movie}},
	}
	if err := s.Refresh(nil, nil, nil, tiers); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := s.Get()
	if !snap.TierWritable[writableDir] {
		t.Errorf("expected %q to be reported writable", writableDir)
	}
	if snap.TierWritable[readOnlyDir] {
		t.Errorf("expected %q to be reported not writable", readOnlyDir)
	}
	if snap.TierWriteError[readOnlyDir] == "" {
		t.Error("expected a reason recorded for why the read-only path can't be written to")
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func fakeRadarrServer(t *testing.T, moviePaths map[int]string) *httptest.Server {
	t.Helper()
	type movie struct {
		ID             int    `json:"id"`
		Title          string `json:"title"`
		Path           string `json:"path"`
		RootFolderPath string `json:"rootFolderPath"`
		Status         string `json:"status"`
	}
	var movies []movie
	for id, path := range moviePaths {
		movies = append(movies, movie{ID: id, Title: "Movie", Path: path, RootFolderPath: "/hot", Status: "released"})
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/movie":
			_ = json.NewEncoder(w).Encode(movies)
		case "/api/v3/tag", "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"records": []}`))
		default:
			t.Errorf("unexpected radarr path %s", r.URL.Path)
		}
	}))
}

func fakeSonarrServer(t *testing.T, seriesPaths map[int]string) *httptest.Server {
	t.Helper()
	type series struct {
		ID             int    `json:"id"`
		Title          string `json:"title"`
		Path           string `json:"path"`
		RootFolderPath string `json:"rootFolderPath"`
		Status         string `json:"status"`
		Ended          bool   `json:"ended"`
	}
	var all []series
	for id, path := range seriesPaths {
		all = append(all, series{ID: id, Title: "Show", Path: path, RootFolderPath: "/hot", Status: "continuing", Ended: false})
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/series":
			_ = json.NewEncoder(w).Encode(all)
		case "/api/v3/tag", "/api/v3/qualityprofile":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v3/queue":
			_, _ = w.Write([]byte(`{"records": []}`))
		default:
			t.Errorf("unexpected sonarr path %s", r.URL.Path)
		}
	}))
}

// fakeJellyfinServer returns a single-user Jellyfin server whose library
// contains exactly the given paths.
func fakeJellyfinServer(t *testing.T, paths []string) *httptest.Server {
	t.Helper()
	type item struct {
		ID   string `json:"Id"`
		Path string `json:"Path"`
	}
	items := make([]item, len(paths))
	for i, p := range paths {
		items[i] = item{ID: "j" + strconv.Itoa(i), Path: p}
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Users":
			_, _ = w.Write([]byte(`[{"Id": "u1"}]`))
		case "/Users/u1/Items":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": items})
		default:
			t.Errorf("unexpected jellyfin path %s", r.URL.Path)
		}
	}))
}
