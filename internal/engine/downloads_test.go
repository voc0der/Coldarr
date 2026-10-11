package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/importcache"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/qbit"
	"github.com/vocoder/coldarr/internal/torrents"
)

// qbitTorrent is the torrents/info record shape, for fakes.
type qbitTorrent struct {
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
}

func fakeQbitServer(t *testing.T, list func() []qbitTorrent) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/torrents/info" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(list())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// arrDownloads fakes an *arr app's queue and per-download history.
type arrDownloads struct {
	*httptest.Server
	mu       sync.Mutex
	queue    []string
	imported map[string]bool
	asked    []string
	fail     bool
}

func newArrDownloads(t *testing.T, root string) *arrDownloads {
	t.Helper()
	f := &arrDownloads{imported: map[string]bool{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case root + "/queue":
			records := []map[string]string{}
			for _, id := range f.queue {
				records = append(records, map[string]string{"downloadId": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"records": records, "totalRecords": len(records)})
		case root + "/history":
			id := r.URL.Query().Get("downloadId")
			f.asked = append(f.asked, id)
			records := []map[string]string{}
			if f.imported[id] {
				records = append(records, map[string]string{"downloadId": id, "eventType": "downloadFolderImported"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"records": records})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// askCounts tallies history lookups by download ID - they run
// concurrently, so their order means nothing.
func askCounts(asks []string) map[string]int {
	counts := map[string]int{}
	for _, a := range asks {
		counts[a]++
	}
	return counts
}

func (f *arrDownloads) historyAsks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

type downloadsFixture struct {
	engine          *Engine
	ssd, seed       string
	radarr          *arrDownloads
	importCachePath string
}

func newDownloadsFixture(t *testing.T, list func(ssd, seed string) []qbitTorrent) *downloadsFixture {
	t.Helper()
	root := t.TempDir()
	f := &downloadsFixture{ssd: filepath.Join(root, "downloads"), seed: filepath.Join(root, "seed")}
	for _, d := range []string{f.ssd, f.seed} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	qb := fakeQbitServer(t, func() []qbitTorrent { return list(f.ssd, f.seed) })
	f.radarr = newArrDownloads(t, "/api/v3")
	f.importCachePath = filepath.Join(root, "imports.json")
	imports, err := importcache.Load(f.importCachePath)
	if err != nil {
		t.Fatal(err)
	}
	f.engine = &Engine{
		Cfg: &config.Config{
			Tiers: []model.Tier{
				{Name: "ssd", Domain: model.DomainDownloads, Role: model.RoleHot, Paths: []string{f.ssd}},
				{Name: "seed", Domain: model.DomainDownloads, Role: model.RoleCold, Paths: []string{f.seed}, TargetUsedPercent: 92, MaxUsedPercent: 97},
			},
			Torrents: config.TorrentsConfig{SettleDays: 3, SeedLabel: "seed", Labels: []config.LabelRule{
				{Label: "movies", Action: config.LabelMove, ImportedBy: "radarr"},
				{Label: "manual", Action: config.LabelMove},
			}},
		},
		Qbit:    qbit.NewClient(qb.URL, qbit.Auth{}),
		Radarr:  arrapi.NewRadarrClient(f.radarr.URL, "key"),
		Imports: imports,
	}
	return f
}

func finished(hash, name, category, savePath string, completedAgo time.Duration) qbitTorrent {
	return qbitTorrent{
		Hash: hash, Name: name, State: "stalledUP", Category: category,
		SavePath: savePath, ContentPath: filepath.Join(savePath, name),
		CompletionOn: time.Now().Add(-completedAgo).Unix(), SeedingTime: int64(completedAgo.Seconds()),
		Size: 1 << 20, Progress: 1,
	}
}

func mkContent(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o750); err != nil {
		t.Fatal(err)
	}
}

func stateOf(t *testing.T, inv *DownloadsInventory, hash string) *torrents.Download {
	t.Helper()
	for _, d := range inv.Downloads {
		if d.Hash == hash {
			return d
		}
	}
	t.Fatalf("no download %s", hash)
	return nil
}

func TestBuildDownloadsInventory_ClassifiesAndConfirmsImports(t *testing.T) {
	week := 7 * 24 * time.Hour
	f := newDownloadsFixture(t, func(ssd, seed string) []qbitTorrent {
		return []qbitTorrent{
			finished("aaa", "Movie.Imported", "movies", ssd, week),
			finished("bbb", "Movie.NotYet", "movies", ssd, week),
			finished("ccc", "Movie.Queued", "movies", ssd, week),
			finished("ddd", "Linux.ISO", "manual", ssd, week),
			finished("eee", "Fresh.ISO", "manual", ssd, time.Hour),
			finished("fff", "Parked", "seed", seed, week),
		}
	})
	for _, n := range []string{"Movie.Imported", "Movie.NotYet", "Movie.Queued", "Linux.ISO", "Fresh.ISO"} {
		mkContent(t, filepath.Join(f.ssd, n))
	}
	mkContent(t, filepath.Join(f.seed, "Parked"))
	f.radarr.queue = []string{"CCC"}
	f.radarr.imported["AAA"] = true

	now := time.Now()

	// Without live checks, nothing is asked: imports not yet confirmed
	// stay held, as on the dashboard.
	inv, err := f.engine.BuildDownloadsInventory(now, false)
	if err != nil {
		t.Fatal(err)
	}
	if asks := f.radarr.historyAsks(); len(asks) != 0 {
		t.Fatalf("history asked %v without live checks", asks)
	}
	if d := stateOf(t, inv, "aaa"); d.State != torrents.Held || !strings.Contains(d.Reason, "not yet checked") {
		t.Errorf("aaa without live checks = %s (%s)", d.State, d.Reason)
	}

	inv, err = f.engine.BuildDownloadsInventory(now, true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]torrents.State{"aaa": torrents.Ready, "bbb": torrents.Held, "ccc": torrents.Active, "ddd": torrents.Ready, "eee": torrents.Waiting, "fff": torrents.Parked}
	for hash, state := range want {
		if d := stateOf(t, inv, hash); d.State != state {
			t.Errorf("%s = %s (%s), want %s", hash, d.State, d.Reason, state)
		}
	}
	if asks := askCounts(f.radarr.historyAsks()); len(asks) != 2 || asks["AAA"] != 1 || asks["BBB"] != 1 {
		t.Errorf("history asked about %v, want only the settled downloads not in the queue, upper-cased", asks)
	}
	counts := inv.Count()
	if counts[torrents.Ready] != 2 || counts[torrents.Parked] != 1 {
		t.Errorf("counts = %v", counts)
	}

	// The confirmation is remembered; the "not yet" isn't.
	inv, err = f.engine.BuildDownloadsInventory(now, true)
	if err != nil {
		t.Fatal(err)
	}
	if asks := askCounts(f.radarr.historyAsks()); asks["AAA"] != 1 || asks["BBB"] != 2 {
		t.Errorf("history asked %v, want AAA never asked again", asks)
	}
	if stateOf(t, inv, "aaa").State != torrents.Ready {
		t.Error("a remembered confirmation should keep it ready")
	}
}

// TestBuildDownloadsInventory_FailsClosed: without qBittorrent's list, or
// any *arr app's queue, there's no telling what's mid-import - so there's
// no inventory to plan from.
func TestBuildDownloadsInventory_FailsClosed(t *testing.T) {
	f := newDownloadsFixture(t, func(ssd, seed string) []qbitTorrent { return nil })
	f.radarr.fail = true
	if _, err := f.engine.BuildDownloadsInventory(time.Now(), true); err == nil || !strings.Contains(err.Error(), "reading radarr's queue") {
		t.Errorf("err = %v, want the queue failure", err)
	}

	f.radarr.fail = false
	f.engine.Qbit = qbit.NewClient("http://127.0.0.1:1", qbit.Auth{})
	if _, err := f.engine.BuildDownloadsInventory(time.Now(), true); err == nil || !strings.Contains(err.Error(), "listing qBittorrent's downloads") {
		t.Errorf("err = %v, want the listing failure", err)
	}

	f.engine.Qbit = nil
	if _, err := f.engine.BuildDownloadsInventory(time.Now(), true); !errors.Is(err, ErrQbitNotConnected) {
		t.Errorf("err = %v, want ErrQbitNotConnected", err)
	}
	if f.engine.DownloadsEnabled() {
		t.Error("downloads aren't enabled without qBittorrent")
	}
}

// TestBuildDownloadsInventory_ImportCheckErrorsHoldTheDownload: a history
// lookup that fails holds that download back, naming the app - it doesn't
// fail everything else.
func TestBuildDownloadsInventory_ImportCheckErrorsHoldTheDownload(t *testing.T) {
	f := newDownloadsFixture(t, func(ssd, seed string) []qbitTorrent {
		return []qbitTorrent{finished("aaa", "Movie", "movies", ssd, 7*24*time.Hour)}
	})
	mkContent(t, filepath.Join(f.ssd, "Movie"))
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/queue") {
			_, _ = w.Write([]byte(`{"records": [], "totalRecords": 0}`))
			return
		}
		http.Error(w, "history unavailable", http.StatusServiceUnavailable)
	}))
	defer broken.Close()
	f.engine.Radarr = arrapi.NewRadarrClient(broken.URL, "key")

	inv, err := f.engine.BuildDownloadsInventory(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if d := stateOf(t, inv, "aaa"); d.State != torrents.Held || !strings.Contains(d.Reason, "couldn't ask Radarr") {
		t.Errorf("aaa = %s (%s), want held naming Radarr", d.State, d.Reason)
	}
}

// TestBuildDownloadsInventory_UnseenMount: finished downloads qBittorrent
// lists on a path Coldarr can't see at all point to a drive mounted
// somewhere else in Coldarr - the path is marked, and a warning says so.
func TestBuildDownloadsInventory_UnseenMount(t *testing.T) {
	f := newDownloadsFixture(t, func(ssd, seed string) []qbitTorrent {
		return []qbitTorrent{
			finished("aaa", "Visible", "manual", ssd, time.Hour),
			finished("bbb", "Ghost1", "seed", seed, time.Hour),
			finished("ccc", "Ghost2", "seed", seed, time.Hour),
		}
	})
	mkContent(t, filepath.Join(f.ssd, "Visible"))

	inv, err := f.engine.BuildDownloadsInventory(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Unseen[f.seed] || inv.Unseen[f.ssd] {
		t.Errorf("Unseen = %v, want only %s", inv.Unseen, f.seed)
	}
	if len(inv.Warnings) != 1 || !strings.Contains(inv.Warnings[0], "can't see any of them") {
		t.Errorf("warnings = %v", inv.Warnings)
	}
}

func TestTorrentMovesInFlight(t *testing.T) {
	states := []string{"stalledUP"}
	var mu sync.Mutex
	qb := fakeQbitServer(t, func() []qbitTorrent {
		mu.Lock()
		defer mu.Unlock()
		var out []qbitTorrent
		for i, s := range states {
			out = append(out, qbitTorrent{Hash: string(rune('a' + i)), Name: "T" + string(rune('A'+i)), State: s})
		}
		return out
	})
	e := &Engine{Qbit: qbit.NewClient(qb.URL, qbit.Auth{})}

	if busy, _, err := e.TorrentMovesInFlight(); err != nil || busy {
		t.Fatalf("idle = %v, %v", busy, err)
	}
	mu.Lock()
	states = []string{"moving", "checkingUP", "checkingResumeData", "allocating", "stalledUP"}
	mu.Unlock()
	busy, detail, err := e.TorrentMovesInFlight()
	if err != nil || !busy || !strings.Contains(detail, "4 download(s) (TA, TB, TC, ...)") {
		t.Fatalf("busy = %v, %q, %v", busy, detail, err)
	}
	if busy, detail, err := e.MovesInFlight(); err != nil || !busy || !strings.Contains(detail, "qBittorrent is still moving") {
		t.Errorf("MovesInFlight = %v, %q, %v; want qBittorrent's moves counted", busy, detail, err)
	}

	// Unreachable is an error, never "idle".
	e.Qbit = qbit.NewClient("http://127.0.0.1:1", qbit.Auth{})
	if _, _, err := e.TorrentMovesInFlight(); err == nil {
		t.Error("an unreachable qBittorrent must be an error")
	}
	if _, _, err := e.MovesInFlight(); err == nil {
		t.Error("MovesInFlight must refuse when qBittorrent can't be checked")
	}

	e.Qbit = nil
	if busy, _, err := e.TorrentMovesInFlight(); err != nil || busy {
		t.Error("no qBittorrent, nothing in flight")
	}
}

func TestReservations(t *testing.T) {
	root := t.TempDir()
	ssd := filepath.Join(root, "downloads")
	mkContent(t, filepath.Join(ssd, "Partial"))
	dev, err := diskusage.DeviceID(ssd)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]PathStatus{
		ssd:          {Path: ssd, DeviceID: dev, DeviceIDKnown: true, Usage: diskusage.Usage{UsedBytes: 100, FreeBytes: 900}},
		"/elsewhere": {Path: "/elsewhere", Usage: diskusage.Usage{UsedBytes: 100, FreeBytes: 900}},
		"/dead":      {Path: "/dead", Err: os.ErrNotExist},
	}
	downloads := []qbit.Torrent{
		{Name: "Partial", State: "downloading", SavePath: ssd, ContentPath: filepath.Join(ssd, "Partial"), AmountLeft: 300, Progress: 0.5},
		{Name: "Not started", State: "queuedDL", SavePath: ssd, ContentPath: filepath.Join(ssd, "Not started"), AmountLeft: 50},
		{Name: "Paused", State: "stoppedDL", SavePath: ssd, AmountLeft: 1000},
		{Name: "Errored", State: "error", SavePath: ssd, AmountLeft: 1000},
		{Name: "Done", State: "stalledUP", SavePath: ssd, Progress: 1},
		{Name: "Unplaceable", State: "downloading", SavePath: "/nowhere/at/all", AmountLeft: 1000},
		{Name: "By path only", State: "downloading", SavePath: "/elsewhere/sub", AmountLeft: 70},
	}
	got := reservations(downloads, status)
	if got[ssd] != 350 || got["/elsewhere"] != 70 || len(got) != 2 {
		t.Fatalf("reservations = %v, want 350 on the download drive and 70 placed by path", got)
	}

	usage := applyReservations(map[string]diskusage.Usage{ssd: status[ssd].Usage, "/elsewhere": {UsedBytes: 10, FreeBytes: 20}}, got)
	if u := usage[ssd]; u.UsedBytes != 450 || u.FreeBytes != 550 || u.UsedPercent != 45 {
		t.Errorf("reserved usage = %+v", u)
	}
	if u := usage["/elsewhere"]; u.UsedBytes != 80 || u.FreeBytes != 0 {
		t.Errorf("a reservation larger than the free space = %+v, want free floored at zero", u)
	}
	if same := applyReservations(usage, nil); len(same) != len(usage) {
		t.Error("no reservations, usage unchanged")
	}

	notes := reservationNotes(map[string]int64{ssd: 3 << 30}, []model.Tier{{Paths: []string{ssd, "/other"}}})
	if len(notes) != 1 || !strings.Contains(notes[0], "3.0 GB is held back on "+ssd) {
		t.Errorf("notes = %v", notes)
	}
}

// TestBuildInventory_ReservesSpaceForDownloadsInProgress: the library plan
// holds back what qBittorrent is still due to write to a tier's disk, and
// says so; qBittorrent being unreachable is only a warning there, since
// every plan and apply already refuses through MovesInFlight.
func TestBuildInventory_ReservesSpaceForDownloadsInProgress(t *testing.T) {
	hotDir := testTierDirs(t)
	mkContent(t, filepath.Join(hotDir, "downloads", "Partial"))
	qb := fakeQbitServer(t, func() []qbitTorrent {
		return []qbitTorrent{{Hash: "a", Name: "Partial", State: "downloading", SavePath: filepath.Join(hotDir, "downloads"), ContentPath: filepath.Join(hotDir, "downloads", "Partial"), AmountLeft: 2 << 30, Progress: 0.1}}
	})
	e := &Engine{
		Cfg:     &config.Config{Tiers: []model.Tier{{Name: "hot", Role: model.RoleHot, Paths: []string{hotDir}, Media: []model.MediaType{model.Movie}}}},
		Qbit:    qbit.NewClient(qb.URL, qbit.Auth{}),
		History: testHistory(t),
	}
	now := time.Now()
	inv, err := e.BuildInventory(now)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Reserved[hotDir] != 2<<30 {
		t.Fatalf("Reserved = %v, want 2 GiB on %s", inv.Reserved, hotDir)
	}
	plan, err := e.BuildPlan(inv, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "2.0 GB is held back on "+hotDir) {
		t.Errorf("plan warnings = %v, want the reservation explained", plan.Warnings)
	}
	if got, raw := plan.FinalUsage[hotDir].UsedBytes, inv.PathStatus[hotDir].Usage.UsedBytes; got != raw+2<<30 {
		t.Errorf("projected used = %d, want the reservation on top of %d", got, raw)
	}

	e.Qbit = qbit.NewClient("http://127.0.0.1:1", qbit.Auth{})
	inv, err = e.BuildInventory(now)
	if err != nil {
		t.Fatalf("an unreachable qBittorrent must not fail the library inventory: %v", err)
	}
	if len(inv.Warnings) == 0 || !strings.Contains(inv.Warnings[0], "Couldn't read qBittorrent's downloads") {
		t.Errorf("warnings = %v, want the failure noted", inv.Warnings)
	}
}

// TestWedged: seed storage counts as wedged only when what Coldarr doesn't
// manage fills it past its max on its own.
func TestWedged(t *testing.T) {
	seedTier := model.Tier{Name: "seed", Domain: model.DomainDownloads, Role: model.RoleCold, Paths: []string{"/seed", "/seed2", "/dead"}, MaxUsedPercent: 90}
	inv := &DownloadsInventory{
		Tiers: []model.Tier{{Name: "ssd", Domain: model.DomainDownloads, Role: model.RoleHot, Paths: []string{"/downloads"}}, seedTier},
		PathStatus: map[string]PathStatus{
			"/downloads": {Usage: diskusage.Usage{UsedBytes: 99, FreeBytes: 1}},
			"/seed":      {Usage: diskusage.Usage{UsedBytes: 95, FreeBytes: 5}},
			"/seed2":     {Usage: diskusage.Usage{UsedBytes: 95, FreeBytes: 5}},
			"/dead":      {Err: os.ErrNotExist},
		},
		Downloads: []*torrents.Download{
			// /seed: 50 of its 95 used bytes are Coldarr's to manage.
			{Torrent: qbit.Torrent{Size: 50}, TierPath: "/seed", State: torrents.Parked},
			// /seed2: only an ignored download, which can't be counted on.
			{Torrent: qbit.Torrent{Size: 50}, TierPath: "/seed2", State: torrents.Ignored},
		},
	}
	got := inv.Wedged()
	if len(got) != 1 || got["/seed2"] != 95 {
		t.Fatalf("Wedged = %v, want only /seed2, with all 95 bytes unmanaged", got)
	}
}
