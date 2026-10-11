package torrents

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/qbit"
)

type execTorrent struct {
	Hash       string  `json:"hash"`
	Name       string  `json:"name"`
	State      string  `json:"state"`
	SavePath   string  `json:"save_path"`
	Progress   float64 `json:"progress"`
	AmountLeft int64   `json:"amount_left"`
	AutoTMM    bool    `json:"auto_tmm"`
}

// execQbit is a qBittorrent whose torrent list the test sets directly, and
// which records every write.
type execQbit struct {
	*httptest.Server
	mu         sync.Mutex
	torrents   map[string]*execTorrent
	categories map[string]bool
	posts      []string
	failPath   string
	failCode   int
}

func newExecQbit(t *testing.T, torrents ...execTorrent) *execQbit {
	t.Helper()
	f := &execQbit{torrents: map[string]*execTorrent{}, categories: map[string]bool{}}
	for i := range torrents {
		tor := torrents[i]
		f.torrents[tor.Hash] = &tor
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodPost {
			f.posts = append(f.posts, strings.TrimPrefix(r.URL.Path, "/api/v2/torrents/")+"?"+r.PostForm.Encode())
			if r.URL.Path == f.failPath {
				http.Error(w, "nope", f.failCode)
				return
			}
		}
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			out := []execTorrent{}
			for _, h := range strings.Split(r.URL.Query().Get("hashes"), "|") {
				if t, ok := f.torrents[h]; ok {
					out = append(out, *t)
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/api/v2/torrents/categories":
			out := map[string]map[string]string{}
			for name := range f.categories {
				out[name] = map[string]string{"name": name}
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/api/v2/torrents/createCategory":
			f.categories[r.PostForm.Get("category")] = true
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *execQbit) set(hash string, edit func(*execTorrent)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(f.torrents[hash])
}

func (f *execQbit) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...)
}

// execFS answers Exists and Size from a map of path sizes.
type execFS struct{ sizes map[string]int64 }

func (f execFS) Exists(path string) (bool, error) {
	_, ok := f.sizes[path]
	return ok, nil
}
func (f execFS) Size(path string) (int64, error) { return f.sizes[path], nil }

// seeding is one member of the test album's cross-seed pair.
func seeding(hash, savePath string) execTorrent {
	return execTorrent{Hash: hash, Name: "Album", State: "stalledUP", SavePath: savePath, Progress: 1}
}

func testEntry() MoveEntry {
	return MoveEntry{
		Name: "Album", Names: []string{"Album", "Album"}, Hashes: []string{"a", "b"}, SizeBytes: 1000,
		FromPath: "/downloads", ContentPath: "/downloads/Album",
		Location: "/seed/music", NewContentPath: "/seed/music/Album", ToPath: "/seed",
	}
}

func newExecutor(t *testing.T, qb *execQbit, fs execFS) (*Executor, *history.DownloadLog) {
	t.Helper()
	log, err := history.LoadDownloadLog(filepath.Join(t.TempDir(), "downloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Executor{Qbit: qbit.NewClient(qb.URL, qbit.Auth{}), SeedLabel: "seed", FS: fs, Log: log}, log
}

func TestExecutor_MoveRechecksBeforeMoving(t *testing.T) {
	qb := newExecQbit(t, seeding("a", "/downloads"), seeding("b", "/downloads"))
	x, _ := newExecutor(t, qb, execFS{})
	e := testEntry()
	e.DisableAutoTMM = []string{"b"}
	qb.set("b", func(t *execTorrent) { t.AutoTMM = true })

	if err := x.Move(e); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got := strings.Join(qb.written(), "\n"); got != "setAutoManagement?enable=false&hashes=b\nsetLocation?hashes=a%7Cb&location=%2Fseed%2Fmusic" {
		t.Errorf("writes =\n%s\nwant AutoTMM switched off, then one setLocation for the pair", got)
	}

	// Each change since the plan skips the move, asking nothing of
	// qBittorrent - so the rest of the run can carry on.
	tests := []struct {
		name    string
		edit    func(*execTorrent)
		wantErr string
	}{
		{name: "downloading again", edit: func(t *execTorrent) { t.State, t.Progress, t.AmountLeft = "downloading", 0.9, 10 }, wantErr: "isn't finished right now (qBittorrent reports downloading) - it moves on a later run, once it's done"},
		{name: "being checked", edit: func(t *execTorrent) { t.State = "checkingUP" }, wantErr: "isn't finished right now"},
		{name: "moved by hand", edit: func(t *execTorrent) { t.SavePath = "/elsewhere" }, wantErr: "has moved since the plan was made"},
		{name: "AutoTMM switched on", edit: func(t *execTorrent) { t.AutoTMM = true }, wantErr: "Automatic Torrent Management was switched on"},
	}
	for _, tt := range tests {
		qb := newExecQbit(t, seeding("a", "/downloads"), seeding("b", "/downloads"))
		qb.set("a", tt.edit)
		x, _ := newExecutor(t, qb, execFS{})
		err := x.Move(testEntry())
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !x.Rejected(err) {
			t.Errorf("%s: err = %v (rejected %v), want a skip containing %q", tt.name, err, err != nil && x.Rejected(err), tt.wantErr)
		}
		if len(qb.written()) != 0 {
			t.Errorf("%s: a skipped move wrote %v", tt.name, qb.written())
		}
	}

	gone := newExecQbit(t, seeding("a", "/downloads"))
	x, _ = newExecutor(t, gone, execFS{})
	if err := x.Move(testEntry()); err == nil || !strings.Contains(err.Error(), "no longer in qBittorrent") {
		t.Errorf("a member gone: err = %v", err)
	}
}

func TestExecutor_MoveFailures(t *testing.T) {
	qb := newExecQbit(t, seeding("a", "/downloads"), seeding("b", "/downloads"))
	qb.failPath, qb.failCode = "/api/v2/torrents/setLocation", http.StatusConflict
	x, _ := newExecutor(t, qb, execFS{})
	if err := x.Move(testEntry()); err == nil || !x.Rejected(err) {
		t.Errorf("a 409 from setLocation = %v, want it counted as refused", err)
	}

	// Unreachable before the move: nothing was asked, so it's a skip.
	x.Qbit = qbit.NewClient("http://127.0.0.1:1", qbit.Auth{})
	if err := x.Move(testEntry()); err == nil || !x.Rejected(err) {
		t.Errorf("unreachable before the move = %v, want a skip", err)
	}
	if x.Rejected(errors.New("connection reset mid-request")) {
		t.Error("an answer lost to the network is ambiguous, never a refusal")
	}
}

func TestExecutor_ConfirmLanded(t *testing.T) {
	e := testEntry()
	landedFS := execFS{sizes: map[string]int64{"/seed/music/Album": 1000}}

	qb := newExecQbit(t, seeding("a", "/downloads"), seeding("b", "/downloads"))
	x, _ := newExecutor(t, qb, landedFS)

	notYet := func(want string) {
		t.Helper()
		landed, _, err := x.ConfirmLanded(e)
		if landed || err == nil || errors.Is(err, mover.ErrMoveFailed) || !strings.Contains(err.Error(), want) {
			t.Fatalf("ConfirmLanded = %v, %v; want not yet: %q", landed, err, want)
		}
	}
	failed := func(want string) {
		t.Helper()
		if landed, _, err := x.ConfirmLanded(e); landed || !errors.Is(err, mover.ErrMoveFailed) || !strings.Contains(err.Error(), want) {
			t.Fatalf("ConfirmLanded = %v, %v; want a final failure: %q", landed, err, want)
		}
	}

	notYet("still has Album at /downloads") // queued: nothing has happened yet
	qb.set("a", func(t *execTorrent) { t.State = "moving" })
	notYet("still moving")
	qb.set("a", func(t *execTorrent) { t.State = "checkingUP" })
	notYet("checking")
	qb.set("a", func(t *execTorrent) { t.State = "stalledUP" })
	failed("stopped moving Album and left it at /downloads") // seen moving, now back

	x, _ = newExecutor(t, qb, landedFS)
	for _, h := range []string{"a", "b"} {
		qb.set(h, func(t *execTorrent) { t.SavePath = "/seed/music/" })
	}
	if landed, path, err := x.ConfirmLanded(e); !landed || path != "/seed/music/Album" || err != nil {
		t.Fatalf("landed = %v %q %v", landed, path, err)
	}

	short := execFS{sizes: map[string]int64{"/seed/music/Album": 500}}
	x, _ = newExecutor(t, qb, short)
	notYet("500 bytes at /seed/music/Album so far")

	// qBittorrent says done, Coldarr never sees it: the mount is wrong.
	x, _ = newExecutor(t, qb, execFS{})
	notYet("can't see /seed/music/Album yet")
	notYet("can't see")
	failed("is /seed mounted into Coldarr from the same drive")

	qb.set("b", func(t *execTorrent) { t.Progress, t.AmountLeft = 0.5, 500 })
	failed("50% complete after the move")
	qb.set("b", func(t *execTorrent) { t.State = "missingFiles" })
	failed("missingFiles")
	qb.mu.Lock()
	delete(qb.torrents, "b")
	qb.mu.Unlock()
	failed("no longer has Album")

	x.Qbit = qbit.NewClient("http://127.0.0.1:1", qbit.Auth{})
	notYet("asking qBittorrent about Album")
}

func TestExecutor_LabelsOnlyAfterLanding(t *testing.T) {
	qb := newExecQbit(t, seeding("a", "/seed/music"), seeding("b", "/seed/music"))
	x, log := newExecutor(t, qb, execFS{})

	if reported, err := x.Landed(testEntry(), "/seed/music/Album"); err != nil || reported {
		t.Fatalf("Landed = %v, %v", reported, err)
	}
	if got := strings.Join(qb.written(), "\n"); got != "createCategory?category=seed&savePath=\nsetCategory?category=seed&hashes=a%7Cb" {
		t.Errorf("writes =\n%s\nwant the seed category created (no save path) and applied to both", got)
	}
	if ev := log.Recent(1); len(ev) != 1 || ev[0].Action != history.DownloadMoved || ev[0].ToPath != "/seed/music/Album" || strings.Join(ev[0].Hashes, ",") != "a,b" {
		t.Errorf("logged = %+v", ev)
	}

	// The category exists now: only the relabel is sent.
	if err := x.Relabel(Relabel{Name: "Album", Hash: "a", Category: "music", DisableAutoTMM: true}); err != nil {
		t.Fatal(err)
	}
	writes := qb.written()
	if got := strings.Join(writes[2:], "\n"); got != "setAutoManagement?enable=false&hashes=a\nsetCategory?category=seed&hashes=a" {
		t.Errorf("relabel writes =\n%s", got)
	}
	if ev := log.Recent(1); ev[0].Action != history.DownloadRelabeled || !strings.Contains(ev[0].Detail, `"music" to "seed"`) {
		t.Errorf("logged = %+v", ev[0])
	}

	// A failed relabel still counts the move as landed - the error says
	// the next run tries again.
	qb.failPath, qb.failCode = "/api/v2/torrents/setCategory", http.StatusConflict
	if _, err := x.Landed(testEntry(), "/seed/music/Album"); err == nil || !strings.Contains(err.Error(), "the next run tries again") {
		t.Errorf("failed relabel = %v", err)
	}
	if err := x.Relabel(Relabel{Name: "Album", Hash: "a"}); err == nil {
		t.Error("a failed relabel must be reported")
	}
	qb.failPath = "/api/v2/torrents/setAutoManagement"
	if err := x.Relabel(Relabel{Name: "Album", Hash: "a", DisableAutoTMM: true}); err == nil || !strings.Contains(err.Error(), "Automatic Torrent Management") {
		t.Errorf("failed AutoTMM switch = %v", err)
	}
	qb.failPath = "/api/v2/torrents/createCategory"
	qb.mu.Lock()
	qb.categories = map[string]bool{}
	qb.mu.Unlock()
	if err := x.Relabel(Relabel{Name: "Album", Hash: "a"}); err == nil || !strings.Contains(err.Error(), "creating the \"seed\" category") {
		t.Errorf("failed category creation = %v", err)
	}
	x.Qbit = qbit.NewClient("http://127.0.0.1:1", qbit.Auth{})
	if err := x.Relabel(Relabel{Name: "Album", Hash: "a"}); err == nil || !strings.Contains(err.Error(), "reading qBittorrent's categories") {
		t.Errorf("unreachable = %v", err)
	}
}
