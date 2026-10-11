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
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/secrets"
)

// fakeTorrent is one download in the fake qBittorrent, in the shape
// /api/v2/torrents/info reports it.
type fakeTorrent struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	State        string  `json:"state"`
	Category     string  `json:"category"`
	Tags         string  `json:"tags"`
	SavePath     string  `json:"save_path"`
	ContentPath  string  `json:"content_path"`
	CompletionOn int64   `json:"completion_on"`
	SeedingTime  int64   `json:"seeding_time"`
	AmountLeft   int64   `json:"amount_left"`
	Size         int64   `json:"size"`
	Progress     float64 `json:"progress"`
	AutoTMM      bool    `json:"auto_tmm"`
	Tracker      string  `json:"tracker"`
}

// fakeQbittorrent serves the slice of qBittorrent's WebUI API Coldarr uses,
// without authentication (as when its WebUI skips it for local clients).
// setLocation really moves the download's content on disk, after reporting
// it as "moving" for a moment, so apply runs, settles and confirms for real.
type fakeQbittorrent struct {
	*httptest.Server

	mu         sync.Mutex
	torrents   []*fakeTorrent
	categories map[string]string
	tags       []string
	trackers   map[string][]string
	posts      []string
	// moveDelay is how long a download stays "moving" before it lands.
	moveDelay time.Duration
	// failMoves makes setLocation answer 409, as qBittorrent does when it
	// can't create the destination.
	failMoves bool
	// unseenMoves lands moves somewhere other than where they were asked
	// to go - what a mismatched mount looks like from Coldarr's side.
	unseenMoves string
	// dropDeletes answers delete requests without removing anything - a
	// request qBittorrent accepted but never carried out.
	dropDeletes bool
}

func newFakeQbittorrent(t *testing.T) *fakeQbittorrent {
	t.Helper()
	f := &fakeQbittorrent{categories: map[string]string{}, trackers: map[string][]string{}, moveDelay: 30 * time.Millisecond}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// add records a download. A finished one (progress 1) also gets its
// content created on disk, sized as the torrent says, so moves and
// visibility checks have something real to work with.
func (f *fakeQbittorrent) add(t *testing.T, tor fakeTorrent) {
	t.Helper()
	if tor.ContentPath == "" {
		tor.ContentPath = filepath.Join(tor.SavePath, tor.Name)
	}
	if tor.State == "" {
		tor.State = "stalledUP"
	}
	if tor.Progress >= 1 {
		if !strings.HasPrefix(tor.ContentPath, os.TempDir()+string(filepath.Separator)) {
			t.Fatalf("a finished fake download gets content on disk, so it must be in a temp dir, not %s", tor.ContentPath)
		}
		if err := os.MkdirAll(tor.ContentPath, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tor.ContentPath, "data.bin"), make([]byte, tor.Size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.torrents = append(f.torrents, &tor)
}

func (f *fakeQbittorrent) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPost {
		f.posts = append(f.posts, strings.TrimPrefix(r.URL.Path, "/api/v2/torrents/")+"?"+r.PostForm.Encode())
	}

	switch r.URL.Path {
	case "/api/v2/app/version":
		_, _ = w.Write([]byte("v5.2.3"))
	case "/api/v2/torrents/info":
		want := map[string]bool{}
		for _, h := range strings.Split(r.URL.Query().Get("hashes"), "|") {
			if h != "" {
				want[h] = true
			}
		}
		out := []fakeTorrent{}
		for _, t := range f.torrents {
			if len(want) == 0 || want[t.Hash] {
				out = append(out, *t)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case "/api/v2/torrents/categories":
		out := map[string]map[string]string{}
		for name, path := range f.categories {
			out[name] = map[string]string{"name": name, "savePath": path}
		}
		_ = json.NewEncoder(w).Encode(out)
	case "/api/v2/torrents/tags":
		_ = json.NewEncoder(w).Encode(append([]string{}, f.tags...))
	case "/api/v2/torrents/trackers":
		hosts, ok := f.trackers[r.URL.Query().Get("hash")]
		if !ok {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		out := []map[string]any{{"url": "** [DHT] **", "status": 0, "tier": -1}}
		for _, h := range hosts {
			out = append(out, map[string]any{"url": "https://" + h + "/announce/secretpasskey", "status": 2, "tier": 0})
		}
		_ = json.NewEncoder(w).Encode(out)
	case "/api/v2/torrents/createCategory":
		f.categories[r.PostForm.Get("category")] = r.PostForm.Get("savePath")
	case "/api/v2/torrents/setCategory":
		category := r.PostForm.Get("category")
		if _, ok := f.categories[category]; !ok && category != "" {
			http.Error(w, "Incorrect category name", http.StatusConflict)
			return
		}
		for _, t := range f.selected(r) {
			t.Category = category
		}
	case "/api/v2/torrents/setAutoManagement":
		for _, t := range f.selected(r) {
			t.AutoTMM = r.PostForm.Get("enable") == "true"
		}
	case "/api/v2/torrents/setLocation":
		if f.failMoves {
			http.Error(w, "Cannot make save path", http.StatusConflict)
			return
		}
		location := r.PostForm.Get("location")
		for _, t := range f.selected(r) {
			f.startMove(t, location)
		}
	case "/api/v2/torrents/delete":
		if f.dropDeletes {
			return
		}
		deleteFiles := r.PostForm.Get("deleteFiles") == "true"
		selected := map[*fakeTorrent]bool{}
		for _, t := range f.selected(r) {
			selected[t] = true
			if deleteFiles {
				_ = os.RemoveAll(t.ContentPath)
			}
		}
		kept := f.torrents[:0]
		for _, t := range f.torrents {
			if !selected[t] {
				kept = append(kept, t)
			}
		}
		f.torrents = kept
	default:
		http.NotFound(w, r)
	}
}

// selected returns the downloads a POST's hashes name. Must be called with
// mu held.
func (f *fakeQbittorrent) selected(r *http.Request) []*fakeTorrent {
	want := map[string]bool{}
	for _, h := range strings.Split(r.PostForm.Get("hashes"), "|") {
		want[h] = true
	}
	var out []*fakeTorrent
	for _, t := range f.torrents {
		if want[t.Hash] {
			out = append(out, t)
		}
	}
	return out
}

// startMove reports the download as moving, then moves its content and
// lands it. Must be called with mu held.
func (f *fakeQbittorrent) startMove(t *fakeTorrent, location string) {
	t.State = "moving"
	from := t.ContentPath
	landAt := location
	if f.unseenMoves != "" {
		landAt = f.unseenMoves
	}
	to := filepath.Join(landAt, filepath.Base(from))
	delay := f.moveDelay
	go func() {
		time.Sleep(delay)
		f.mu.Lock()
		defer f.mu.Unlock()
		// The fake only ever moves within the test's own temp dirs, at
		// paths its own requests name. A cross-seed partner may have
		// moved the shared files already.
		if _, err := os.Stat(to); err != nil { //nolint:gosec // see above
			_ = os.MkdirAll(filepath.Dir(to), 0o750) //nolint:gosec // see above
			_ = os.Rename(from, to)                  //nolint:gosec // see above
		}
		t.SavePath = location
		t.ContentPath = filepath.Join(location, filepath.Base(from))
		t.State = "stalledUP"
	}()
}

// downloadsTestServer is a Server tiering downloads only: a download drive
// and seed storage in temp directories, a fake qBittorrent, and a fake
// Radarr for its queue and import history.
type downloadsTestServer struct {
	*Server
	qb        *fakeQbittorrent
	radarr    *fakeArrDownloads
	dir       string
	ssd, seed string
}

func newDownloadsTestServer(t *testing.T) *downloadsTestServer {
	t.Helper()
	dir := t.TempDir()
	f := &downloadsTestServer{dir: dir, ssd: filepath.Join(dir, "downloads"), seed: filepath.Join(dir, "seed")}
	for _, d := range []string{f.ssd, f.seed} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Tiers: []model.Tier{
			{Name: "ssd", Domain: model.DomainDownloads, Role: model.RoleHot, Paths: []string{f.ssd}},
			{Name: "seed", Domain: model.DomainDownloads, Role: model.RoleCold, Paths: []string{f.seed}, TargetUsedPercent: 99, MaxUsedPercent: 100},
		},
		History: config.HistoryConfig{Path: filepath.Join(dir, "history.json")},
		Torrents: config.TorrentsConfig{SettleDays: 3, SeedLabel: "seed", Labels: []config.LabelRule{
			{Label: "movies", Action: config.LabelMove, ImportedBy: "radarr"},
			{Label: "manual", Action: config.LabelMove},
			{Label: "transfer", Action: config.LabelIgnore},
		}},
	}
	connStore, err := secrets.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.qb = newFakeQbittorrent(t)
	f.radarr = newFakeArrDownloads(t)
	for app, url := range map[string]string{"qbittorrent": f.qb.URL, "radarr": f.radarr.URL} {
		if err := connStore.Set(app, secrets.Connection{URL: url, APIKey: "key"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("COLDARR_SETTLE_CHECK_INTERVAL", "20ms")
	t.Setenv("COLDARR_SETTLE_STABLE_CHECKS", "1")
	t.Setenv("COLDARR_SETTLE_MAX_WAIT", "3s")
	f.Server, err = New(filepath.Join(dir, "coldarr.yaml"), cfg, connStore)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// finishedDownload is a download that completed long enough ago to have
// settled, on the given drive.
func finishedDownload(hash, name, category, savePath string) fakeTorrent {
	return fakeTorrent{
		Hash: hash, Name: name, Category: category, SavePath: savePath,
		CompletionOn: time.Now().Add(-7 * 24 * time.Hour).Unix(), SeedingTime: 7 * 24 * 3600,
		Size: 4096, Progress: 1,
	}
}

// fakeArrDownloads is the Radarr API as download tiering uses it: a queue
// of download IDs and per-download import history. Library calls answer
// with an empty library.
type fakeArrDownloads struct {
	*httptest.Server
	mu       sync.Mutex
	queue    []string
	imported map[string]bool
}

func newFakeArrDownloads(t *testing.T) *fakeArrDownloads {
	t.Helper()
	f := &fakeArrDownloads{imported: map[string]bool{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/v3/queue":
			records := []map[string]any{}
			for _, id := range f.queue {
				records = append(records, map[string]any{"downloadId": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"records": records, "totalRecords": len(records)})
		case "/api/v3/history":
			id := r.URL.Query().Get("downloadId")
			records := []map[string]any{}
			if f.imported[id] {
				records = append(records, map[string]any{"downloadId": id, "eventType": "downloadFolderImported"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"records": records})
		case "/api/v3/command":
			_ = json.NewEncoder(w).Encode([]any{})
		case "/api/v3/movie", "/api/v3/tag", "/api/v3/qualityprofile":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeQbittorrent) torrent(hash string) fakeTorrent {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.torrents {
		if t.Hash == hash {
			return *t
		}
	}
	return fakeTorrent{}
}

func (f *fakeArrDownloads) setImported(hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imported[strings.ToUpper(hash)] = true
}

// waitDownloadsIdle waits for the downloads run to finish, relabels and
// all.
func waitDownloadsIdle(t *testing.T, srv *Server) {
	t.Helper()
	srv.applyMu.Lock()
	run := srv.currentDownloadsRun
	srv.applyMu.Unlock()
	if run == nil {
		t.Fatal("no downloads run started")
	}
	select {
	case <-run.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the downloads run didn't finish")
	}
}

func (f *fakeQbittorrent) postedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...)
}
