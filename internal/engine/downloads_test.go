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
	"github.com/vocoder/coldarr/internal/trackercache"
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
	// event is the history event an import is recorded as.
	event string
}

func newArrDownloads(t *testing.T, root string) *arrDownloads {
	t.Helper()
	f := &arrDownloads{imported: map[string]bool{}, event: "downloadFolderImported"}
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
				records = append(records, map[string]string{"downloadId": id, "eventType": f.event})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"records": records})
		case root + "/movie":
			_, _ = w.Write([]byte(`[]`))
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

// TestBuildDownloadsInventory_EachImporter: Sonarr's and Lidarr's imports
// are confirmed from their own history and queue - Lidarr's under its own
// API version and event name - and while a label's importer isn't
// connected, its downloads are held, naming the app.
func TestBuildDownloadsInventory_EachImporter(t *testing.T) {
	week := 7 * 24 * time.Hour
	f := newDownloadsFixture(t, func(ssd, seed string) []qbitTorrent {
		return []qbitTorrent{
			finished("aaa", "Show.S01", "tv", ssd, week),
			finished("bbb", "Album", "music", ssd, week),
			finished("ccc", "Show.S02", "tv", ssd, week),
		}
	})
	for _, n := range []string{"Show.S01", "Album", "Show.S02"} {
		mkContent(t, filepath.Join(f.ssd, n))
	}
	f.engine.Cfg.Torrents.Labels = append(f.engine.Cfg.Torrents.Labels,
		config.LabelRule{Label: "tv", Action: config.LabelMove, ImportedBy: "sonarr"},
		config.LabelRule{Label: "music", Action: config.LabelMove, ImportedBy: "lidarr"})

	inv, err := f.engine.BuildDownloadsInventory(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	for hash, app := range map[string]string{"aaa": "Sonarr", "bbb": "Lidarr"} {
		if d := stateOf(t, inv, hash); d.State != torrents.Held || !strings.Contains(d.Reason, app+" isn't connected") {
			t.Errorf("%s with %s not connected = %s (%s)", hash, app, d.State, d.Reason)
		}
	}

	sonarr := newArrDownloads(t, "/api/v3")
	sonarr.imported["AAA"] = true
	sonarr.queue = []string{"CCC"}
	lidarr := newArrDownloads(t, "/api/v1")
	lidarr.event = "downloadImported"
	lidarr.imported["BBB"] = true
	f.engine.Sonarr = arrapi.NewSonarrClient(sonarr.URL, "key")
	f.engine.Lidarr = arrapi.NewLidarrClient(lidarr.URL, "key")

	inv, err = f.engine.BuildDownloadsInventory(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]torrents.State{"aaa": torrents.Ready, "bbb": torrents.Ready, "ccc": torrents.Active}
	for hash, state := range want {
		if d := stateOf(t, inv, hash); d.State != state {
			t.Errorf("%s = %s (%s), want %s", hash, d.State, d.Reason, state)
		}
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
	e := &Engine{
		Cfg:  &config.Config{Tiers: []model.Tier{{Name: "seed", Domain: model.DomainDownloads, Role: model.RoleCold, Paths: []string{"/seed"}}}},
		Qbit: qbit.NewClient(qb.URL, qbit.Auth{}),
	}

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

	// Connected but with no downloads tier, Coldarr never moves anything
	// through qBittorrent, so it isn't asked: even unreachable, it holds
	// nothing up.
	e.Cfg.Tiers = []model.Tier{{Name: "hot", Role: model.RoleHot, Paths: []string{"/media"}}}
	if busy, _, err := e.MovesInFlight(); err != nil || busy {
		t.Errorf("MovesInFlight without download tiering = %v, %v; want qBittorrent left out", busy, err)
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

func TestDownloadsFS(t *testing.T) {
	root := t.TempDir()
	mkContent(t, filepath.Join(root, "Album", "disc1"))
	if err := os.WriteFile(filepath.Join(root, "Album", "a.flac"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Album", "disc1", "b.flac"), make([]byte, 50), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.flac", filepath.Join(root, "Album", "link")); err != nil {
		t.Fatal(err)
	}
	fs := downloadsFS{}

	if n, err := fs.Size(filepath.Join(root, "Album")); err != nil || n != 150 {
		t.Errorf("Size = %d, %v; want 150, symlinks not counted", n, err)
	}
	if n, err := fs.Size(filepath.Join(root, "Album", "a.flac")); err != nil || n != 100 {
		t.Errorf("Size of a file = %d, %v", n, err)
	}
	if ok, err := fs.Exists(filepath.Join(root, "Album")); !ok || err != nil {
		t.Errorf("Exists = %v, %v", ok, err)
	}
	if ok, err := fs.Exists(filepath.Join(root, "Nope")); ok || err != nil {
		t.Errorf("Exists(missing) = %v, %v", ok, err)
	}
	if linked, err := fs.HasHardLinks(filepath.Join(root, "Album")); linked || err != nil {
		t.Errorf("HasHardLinks before linking = %v, %v", linked, err)
	}
	if err := os.Link(filepath.Join(root, "Album", "disc1", "b.flac"), filepath.Join(root, "library-copy.flac")); err != nil {
		t.Fatal(err)
	}
	if linked, err := fs.HasHardLinks(filepath.Join(root, "Album")); !linked || err != nil {
		t.Errorf("HasHardLinks after linking = %v, %v", linked, err)
	}
	if _, err := fs.Size(filepath.Join(root, "Nope")); err == nil {
		t.Error("sizing a missing path is an error")
	}
}

func TestBuildDownloadsPlan(t *testing.T) {
	f := newDownloadsFixture(t, func(ssd, seed string) []qbitTorrent {
		return []qbitTorrent{
			finished("aaa", "Linux.ISO", "manual", ssd, 7*24*time.Hour),
			{Hash: "bbb", Name: "Partial", State: "downloading", Category: "manual", SavePath: ssd, ContentPath: filepath.Join(ssd, "Partial"), AmountLeft: 3 << 30, Progress: 0.5},
		}
	})
	mkContent(t, filepath.Join(f.ssd, "Linux.ISO"))
	mkContent(t, filepath.Join(f.ssd, "Partial"))

	inv, err := f.engine.BuildDownloadsInventory(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	plan := f.engine.BuildDownloadsPlan(inv)
	if len(plan.Entries) != 1 || plan.Entries[0].Name != "Linux.ISO" || plan.Entries[0].NewContentPath != filepath.Join(f.seed, "Linux.ISO") {
		t.Fatalf("entries = %+v", plan.Entries)
	}
	if joined := strings.Join(plan.Warnings, "\n"); !strings.Contains(joined, "3.0 GB is held back on "+f.ssd) {
		t.Errorf("warnings = %v, want the in-progress download's reservation noted", plan.Warnings)
	}

	// Seed storage capped below what's there, none of it Coldarr's.
	f.engine.Cfg.Tiers[1].MaxUsedPercent = 0.0001
	f.engine.Cfg.Tiers[1].TargetUsedPercent = 0.0001
	inv, err = f.engine.BuildDownloadsInventory(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	plan = f.engine.BuildDownloadsPlan(inv)
	if len(plan.Entries) != 0 || !strings.Contains(strings.Join(plan.Warnings, "\n"), f.seed+" is full of content Coldarr can't move") {
		t.Errorf("plan = %+v, want nothing moved and the wedged seed storage named", plan)
	}

	opts := f.engine.DownloadsOptions()
	if !opts.Serial || opts.CheckStorage == nil {
		t.Errorf("DownloadsOptions = %+v, want one move at a time with the storage re-check", opts)
	}
	if x := f.engine.DownloadsExecutor(); x.SeedLabel != "seed" || x.Qbit != f.engine.Qbit {
		t.Errorf("DownloadsExecutor = %+v", x)
	}
}

func TestScanTrackers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/torrents/info":
			_, _ = w.Write([]byte(`[{"hash": "aaa", "name": "A"}, {"hash": "bbb", "name": "B", "tracker": "https://live.example.com/announce/KEY"}]`))
		case "/api/v2/torrents/trackers":
			if r.URL.Query().Get("hash") == "aaa" {
				_, _ = w.Write([]byte(`[{"url": "https://tracker.example.org/announce/KEY"}]`))
				return
			}
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	store, err := trackercache.Load(filepath.Join(t.TempDir(), "trackers.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Qbit: qbit.NewClient(srv.URL, qbit.Auth{}), Trackers: store}

	looked, err := e.ScanTrackers(time.Now())
	if err != nil || looked != 1 {
		t.Fatalf("ScanTrackers = %d, %v; want aaa looked up and the vanished bbb skipped", looked, err)
	}
	if hosts, ok := store.Get().Hosts("aaa"); !ok || len(hosts) != 1 || hosts[0] != "tracker.example.org" {
		t.Errorf("aaa = %v, %v", hosts, ok)
	}
	if seen := store.Get().SeenHosts(); strings.Join(seen, ",") != "live.example.com,tracker.example.org" {
		t.Errorf("seen = %v", seen)
	}

	e.Qbit = qbit.NewClient("http://127.0.0.1:1", qbit.Auth{})
	if _, err := e.ScanTrackers(time.Now()); err == nil || !strings.Contains(err.Error(), "listing qBittorrent's downloads") {
		t.Errorf("unreachable = %v", err)
	}
	e.Qbit = nil
	if _, err := e.ScanTrackers(time.Now()); !errors.Is(err, ErrQbitNotConnected) {
		t.Errorf("no qBittorrent = %v", err)
	}
}

func TestPlanRemovals_Engine(t *testing.T) {
	week := 7 * 24 * time.Hour
	f := newDownloadsFixture(t, func(ssd, seed string) []qbitTorrent {
		return []qbitTorrent{
			finished("aaa", "Done.ISO", "manual", ssd, 30*24*time.Hour),
			finished("bbb", "Young.ISO", "manual", ssd, week),
		}
	})
	mkContent(t, filepath.Join(f.ssd, "Done.ISO"))
	mkContent(t, filepath.Join(f.ssd, "Young.ISO"))
	cache, err := trackercache.Load(filepath.Join(t.TempDir(), "trackers.json"))
	if err != nil {
		t.Fatal(err)
	}
	trackersSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"url": "https://tracker.example.org/announce"}]`))
	}))
	defer trackersSrv.Close()
	if _, err := cache.Refresh(qbit.NewClient(trackersSrv.URL, qbit.Auth{}), []qbit.Torrent{{Hash: "aaa"}, {Hash: "bbb"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.engine.Trackers = cache
	f.engine.Cfg.Torrents.Seeding = config.SeedingConfig{MinSeedDays: 14}

	inv, err := f.engine.BuildDownloadsInventory(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}

	// Off: nothing, whatever's done.
	if plan, err := f.engine.PlanRemovals(inv); err != nil || len(plan.Removals) != 0 {
		t.Fatalf("removal off = %+v, %v", plan, err)
	}

	f.engine.Cfg.Torrents.Removal.Mode = config.RemovalRemove
	plan, err := f.engine.PlanRemovals(inv)
	if err != nil || len(plan.Removals) != 1 || plan.Removals[0].Name != "Done.ISO" {
		t.Fatalf("removals = %+v, %v; want only the download past 14+7 days", plan, err)
	}
	// ...and the move planner leaves it for removal.
	moves := f.engine.BuildDownloadsPlan(inv)
	if len(moves.Entries) != 1 || moves.Entries[0].Name != "Young.ISO" {
		t.Errorf("moves = %+v, want Done.ISO left to be removed", moves.Entries)
	}
	if x := f.engine.Remover(); x.Qbit != f.engine.Qbit || x.Seeding.MinSeedDays != 14 {
		t.Errorf("Remover = %+v", x)
	}

	// The library guard can't run without every library: refuse.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer broken.Close()
	f.engine.Sonarr = arrapi.NewSonarrClient(broken.URL, "key")
	if _, err := f.engine.PlanRemovals(inv); err == nil || !strings.Contains(err.Error(), "reading sonarr's library") {
		t.Errorf("an unreadable library = %v, want a refusal", err)
	}
	// With removal planning failing, moves go ahead as if it were off.
	if moves := f.engine.BuildDownloadsPlan(inv); len(moves.Entries) != 2 {
		t.Errorf("moves with removal planning failing = %+v, want both moved", moves.Entries)
	}
}
