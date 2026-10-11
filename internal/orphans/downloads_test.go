package orphans

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/qbit"
)

type qbitDownload struct {
	Hash        string `json:"hash"`
	Name        string `json:"name"`
	SavePath    string `json:"save_path"`
	ContentPath string `json:"content_path"`
	files       []string
}

// fakeQbit serves a download list and per-download file lists.
type fakeQbit struct {
	*httptest.Server
	mu        sync.Mutex
	downloads []qbitDownload
	failFiles bool
}

func newFakeQbit(t *testing.T, downloads ...qbitDownload) *fakeQbit {
	t.Helper()
	f := &fakeQbit{downloads: downloads}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			_ = json.NewEncoder(w).Encode(f.downloads)
		case "/api/v2/torrents/files":
			if f.failFiles {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			for _, d := range f.downloads {
				if d.Hash == r.URL.Query().Get("hash") {
					var out []map[string]string
					for _, name := range d.files {
						out = append(out, map[string]string{"name": name})
					}
					_ = json.NewEncoder(w).Encode(out)
					return
				}
			}
			http.Error(w, "Not Found", http.StatusNotFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeQbit) add(d qbitDownload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads = append(f.downloads, d)
}

func candidatePaths(snap Snapshot) []string {
	var out []string
	for _, c := range snap.Candidates {
		out = append(out, c.Path)
	}
	sort.Strings(out)
	return out
}

// TestRefresh_DownloadFolders: in a download folder every entry is judged
// against qBittorrent - folders and loose files alike - while what
// qBittorrent accounts for, however it lays it out, never is.
func TestRefresh_DownloadFolders(t *testing.T) {
	dl := t.TempDir()
	lib := t.TempDir()

	mustMkdir(t, filepath.Join(dl, "Multi.File"))                         // a multi-file download
	mustWriteFile(t, filepath.Join(dl, "Single.mkv"), 10)                 // a single-file download
	mustWriteFile(t, filepath.Join(dl, "Partial.mkv.!qB"), 20)            // incomplete, renamed by qBittorrent
	mustMkdir(t, filepath.Join(dl, "movies", "Categorized"))              // in a category subfolder
	mustWriteFile(t, filepath.Join(dl, "movies", "Old.Leftover.mkv"), 30) // removed without its files
	mustMkdir(t, filepath.Join(dl, "loose"))
	mustWriteFile(t, filepath.Join(dl, "loose", "a.mkv"), 1)  // "Don't create subfolder" download
	mustMkdir(t, filepath.Join(dl, "loose", "extras"))        // ...whose files include extras/
	mustWriteFile(t, filepath.Join(dl, "loose", "stray"), 40) // ...beside a stray file
	mustMkdir(t, filepath.Join(dl, "Gone.Folder"))
	mustWriteFile(t, filepath.Join(dl, "Gone.Folder", "x.mkv"), 50)
	if err := os.Symlink("/nowhere", filepath.Join(dl, "dangling")); err != nil {
		t.Fatal(err)
	}
	// A download whose data sits inside a library folder is known too.
	mustMkdir(t, filepath.Join(lib, "Seeding.From.Library"))
	mustMkdir(t, filepath.Join(lib, "Library.Orphan"))

	qb := newFakeQbit(t,
		qbitDownload{Hash: "a", Name: "Multi.File", SavePath: dl, ContentPath: filepath.Join(dl, "Multi.File")},
		qbitDownload{Hash: "b", Name: "Single.mkv", SavePath: dl, ContentPath: filepath.Join(dl, "Single.mkv")},
		qbitDownload{Hash: "c", Name: "Partial.mkv", SavePath: dl, ContentPath: filepath.Join(dl, "Partial.mkv")},
		qbitDownload{Hash: "d", Name: "Categorized", SavePath: filepath.Join(dl, "movies"), ContentPath: filepath.Join(dl, "movies", "Categorized")},
		qbitDownload{Hash: "e", Name: "Loose", SavePath: filepath.Join(dl, "loose"), ContentPath: filepath.Join(dl, "loose"), files: []string{"a.mkv", "extras/sub.srt"}},
		qbitDownload{Hash: "f", Name: "Seeding.From.Library", SavePath: lib, ContentPath: filepath.Join(lib, "Seeding.From.Library")},
		qbitDownload{Hash: "g", Name: "Magnet", SavePath: dl}, // no metadata yet: no content path
	)
	s, err := Load(t.TempDir() + "/orphans.json")
	if err != nil {
		t.Fatal(err)
	}
	tiers := []model.Tier{
		{Name: "ssd", Domain: model.DomainDownloads, Role: model.RoleHot, Paths: []string{dl}},
		{Name: "library", Role: model.RoleHot, Paths: []string{lib}, Media: []model.MediaType{model.Movie}},
	}
	if err := s.Refresh(Sources{Qbit: qbit.NewClient(qb.URL, qbit.Auth{})}, tiers); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := s.Get()
	want := []string{
		filepath.Join(dl, "Gone.Folder"),
		filepath.Join(dl, "dangling"),
		filepath.Join(dl, "loose", "stray"),
		filepath.Join(dl, "movies", "Old.Leftover.mkv"),
		filepath.Join(lib, "Library.Orphan"),
	}
	if got := candidatePaths(snap); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("candidates =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range snap.Candidates {
		switch c.Path {
		case filepath.Join(dl, "Gone.Folder"):
			if !c.IsDownloads() || c.File || c.SizeBytes != 50 || c.TierName != "ssd" {
				t.Errorf("Gone.Folder = %+v", c)
			}
		case filepath.Join(dl, "movies", "Old.Leftover.mkv"):
			if !c.File || c.SizeBytes != 30 {
				t.Errorf("Old.Leftover.mkv = %+v, want a 30-byte loose file", c)
			}
		case filepath.Join(lib, "Library.Orphan"):
			if c.IsDownloads() {
				t.Error("a library folder's orphan isn't a download-folder one")
			}
		}
	}
}

func TestRefresh_DownloadFoldersFailClosed(t *testing.T) {
	dl := t.TempDir()
	tiers := []model.Tier{{Name: "ssd", Domain: model.DomainDownloads, Role: model.RoleHot, Paths: []string{dl}}}
	s, err := Load(t.TempDir() + "/orphans.json")
	if err != nil {
		t.Fatal(err)
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if err := s.Refresh(Sources{Qbit: qbit.NewClient(dead.URL, qbit.Auth{})}, tiers); err == nil || !strings.Contains(err.Error(), "fetching qbittorrent downloads") {
		t.Errorf("unreachable qBittorrent = %v, want the scan failed", err)
	}

	// A loose-layout download whose files can't be listed can't be told
	// apart from the orphans around it, so the scan fails rather than
	// guess.
	qb := newFakeQbit(t, qbitDownload{Hash: "e", Name: "Loose", SavePath: dl, ContentPath: dl, files: []string{"a"}})
	qb.failFiles = true
	if err := s.Refresh(Sources{Qbit: qbit.NewClient(qb.URL, qbit.Auth{})}, tiers); err == nil || !strings.Contains(err.Error(), `files of qbittorrent download "Loose"`) {
		t.Errorf("failed file listing = %v, want the scan failed", err)
	}

	// One removed between the list and the file lookup is simply gone.
	qb2 := newFakeQbit(t, qbitDownload{Hash: "zzz", Name: "Vanished", SavePath: dl, ContentPath: dl})
	qb2.mu.Lock()
	qb2.downloads[0].Hash = "zzz"
	qb2.mu.Unlock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/torrents/files" {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		qb2.Config.Handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	if err := s.Refresh(Sources{Qbit: qbit.NewClient(srv.URL, qbit.Auth{})}, tiers); err != nil {
		t.Errorf("a download removed mid-scan = %v, want it skipped", err)
	}
}

func deletableFixture(t *testing.T, allowDeletes bool) (*Store, *fakeQbit, []model.Tier, string) {
	t.Helper()
	dl := t.TempDir()
	mustMkdir(t, filepath.Join(dl, "Orphan"))
	mustWriteFile(t, filepath.Join(dl, "Orphan", "x.mkv"), 10)
	mustWriteFile(t, filepath.Join(dl, "stray.mkv"), 5)
	mustMkdir(t, filepath.Join(dl, "Seeding"))

	qb := newFakeQbit(t, qbitDownload{Hash: "a", Name: "Seeding", SavePath: dl, ContentPath: filepath.Join(dl, "Seeding")})
	tiers := []model.Tier{{Name: "ssd", Domain: model.DomainDownloads, Role: model.RoleHot, Paths: []string{dl}, AllowDeletes: allowDeletes}}
	s, err := Load(t.TempDir() + "/orphans.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(Sources{Qbit: qbit.NewClient(qb.URL, qbit.Auth{})}, tiers); err != nil {
		t.Fatal(err)
	}
	return s, qb, tiers, dl
}

func TestDeleteDownload(t *testing.T) {
	s, qb, tiers, dl := deletableFixture(t, true)
	src := Sources{Qbit: qbit.NewClient(qb.URL, qbit.Auth{})}

	got, err := s.DeleteDownload(filepath.Join(dl, "Orphan"), src, tiers)
	if err != nil {
		t.Fatalf("DeleteDownload: %v", err)
	}
	if got.SizeBytes != 10 {
		t.Errorf("deleted = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dl, "Orphan")); !os.IsNotExist(err) {
		t.Fatal("the orphan should be gone from disk")
	}
	if paths := candidatePaths(s.Get()); len(paths) != 1 || paths[0] != filepath.Join(dl, "stray.mkv") {
		t.Errorf("candidates after delete = %v, want only the stray file left", paths)
	}
	reloaded, err := Load(s.path)
	if err != nil || len(reloaded.Get().Candidates) != 1 {
		t.Errorf("the snapshot after delete should be saved: %v", err)
	}

	// A file deletes too.
	if _, err := s.DeleteDownload(filepath.Join(dl, "stray.mkv")+"/", src, tiers); err != nil {
		t.Errorf("deleting a loose file: %v", err)
	}
}

func TestDeleteDownload_Guards(t *testing.T) {
	s, qb, tiers, dl := deletableFixture(t, true)
	src := Sources{Qbit: qbit.NewClient(qb.URL, qbit.Auth{})}
	orphan := filepath.Join(dl, "Orphan")

	readOnly := []model.Tier{tiers[0]}
	readOnly[0].AllowDeletes = false
	renamed := []model.Tier{tiers[0]}
	renamed[0].Name = "other"
	library := []model.Tier{tiers[0]}
	library[0].Domain = ""
	elsewhere := []model.Tier{tiers[0]}
	elsewhere[0].Paths = []string{t.TempDir()}

	tests := []struct {
		name    string
		path    string
		src     Sources
		tiers   []model.Tier
		wantErr string
	}{
		{name: "no qBittorrent", path: orphan, src: Sources{}, tiers: tiers, wantErr: "qBittorrent isn't connected"},
		{name: "not from the scan", path: filepath.Join(dl, "Seeding"), src: src, tiers: tiers, wantErr: "isn't on the list from the last scan"},
		{name: "outside the tier", path: "/etc", src: src, tiers: tiers, wantErr: "isn't on the list"},
		{name: "the tier itself", path: dl, src: src, tiers: tiers, wantErr: "isn't on the list"},
		{name: "read-only tier", path: orphan, src: src, tiers: readOnly, wantErr: "is read-only"},
		{name: "tier gone", path: orphan, src: src, tiers: renamed, wantErr: "no longer a downloads tier"},
		{name: "tier now library", path: orphan, src: src, tiers: library, wantErr: "no longer a downloads tier"},
		{name: "tier path changed", path: orphan, src: src, tiers: elsewhere, wantErr: "isn't inside any of tier"},
		{name: "qBittorrent down", path: orphan, src: Sources{Qbit: qbit.NewClient("http://127.0.0.1:1", qbit.Auth{})}, tiers: tiers, wantErr: "checking"},
	}
	for _, tt := range tests {
		if _, err := s.DeleteDownload(tt.path, tt.src, tt.tiers); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: err = %v, want one containing %q", tt.name, err, tt.wantErr)
		}
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("no refused delete may touch the disk")
	}

	// The tier path failing its storage check - here, by no longer being a
	// directory - stops everything, even with the orphan still listed.
	if err := os.Rename(dl, dl+".away"); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, dl, 1)
	if _, err := s.DeleteDownload(orphan, src, tiers); err == nil || !strings.Contains(err.Error(), "refusing to delete from") {
		t.Errorf("unhealthy tier path: err = %v", err)
	}
	if err := os.Remove(dl); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dl+".away", dl); err != nil {
		t.Fatal(err)
	}

	// A download added since the scan now uses the folder.
	qb.add(qbitDownload{Hash: "new", Name: "x.mkv", SavePath: orphan, ContentPath: filepath.Join(orphan, "x.mkv")})
	if _, err := s.DeleteDownload(orphan, src, tiers); err == nil || !strings.Contains(err.Error(), "is in use now") {
		t.Errorf("a folder a new download uses: err = %v", err)
	}

	// Gone from disk since the scan.
	qb.mu.Lock()
	qb.downloads = qb.downloads[:1]
	qb.mu.Unlock()
	if err := os.RemoveAll(orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteDownload(orphan, src, tiers); err == nil || !strings.Contains(err.Error(), "checking") {
		t.Errorf("already gone: err = %v", err)
	}
}
