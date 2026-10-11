package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/secrets"
)

// newDownloadsSettingsServer is a fresh install with a fake qBittorrent
// connected.
func newDownloadsSettingsServer(t *testing.T) (*Server, *fakeQbittorrent) {
	t.Helper()
	srv := newSettingsTestServer(t)
	// What config.LoadForServer fills in on a real install.
	srv.cfg.Torrents = config.TorrentsConfig{SettleDays: 3, SeedLabel: "seed"}
	qb := newFakeQbittorrent(t)
	if err := srv.connStore.Set("qbittorrent", secrets.Connection{URL: qb.URL}); err != nil {
		t.Fatal(err)
	}
	return srv, qb
}

// TestDownloadsSettings_LabelsAreLiveAndSticky: the Labels table lists what
// qBittorrent has right now, plus any saved rule whose label it no longer
// has - kept until someone removes it, since a category can disappear and
// come back.
func TestDownloadsSettings_LabelsAreLiveAndSticky(t *testing.T) {
	srv, qb := newDownloadsSettingsServer(t)
	qb.categories["movies"] = "/downloads/movies"
	qb.categories["tv"] = ""
	qb.tags = []string{"transfer", "tv"}

	data := srv.downloadsSettingsData()
	if data.LabelsError != "" || len(data.Labels) != 3 {
		t.Fatalf("labels = %+v (%q), want movies, transfer and tv", data.Labels, data.LabelsError)
	}
	if data.Labels[2].Label != "tv" || data.Labels[2].Where != "category and tag" {
		t.Errorf("tv row = %+v, want it shown as both a category and a tag", data.Labels[2])
	}

	save := func(form url.Values) string {
		t.Helper()
		return serveForm(srv.handleDownloadsLabelsSave, "/settings/downloads/labels", form).Body.String()
	}
	body := save(url.Values{
		"label_0": {"movies"}, "action_0": {"move"}, "imported_by_0": {"radarr"},
		"label_1": {"transfer"}, "action_1": {"ignore"}, "imported_by_1": {"sonarr"}, // ignored labels import nothing
		"label_2": {"tv"}, "action_2": {""}, "imported_by_2": {"sonarr"}, // no rule: dropped
	})
	if !strings.Contains(body, "Label rules saved.") {
		t.Fatalf("save:\n%s", body)
	}
	want := []config.LabelRule{
		{Label: "movies", Action: config.LabelMove, ImportedBy: "radarr"},
		{Label: "transfer", Action: config.LabelIgnore},
	}
	if got := srv.currentConfig().Torrents.Labels; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("saved rules = %+v, want %+v", got, want)
	}

	// movies vanishes from qBittorrent: its rule stays, flagged.
	delete(qb.categories, "movies")
	data = srv.downloadsSettingsData()
	last := data.Labels[len(data.Labels)-1]
	if last.Label != "movies" || last.Where != "" || last.Action != "move" {
		t.Fatalf("the vanished label's row = %+v, want its saved rule kept", last)
	}
	page := httptest.NewRecorder()
	srv.handleDownloadsSettingsPage(page, httptest.NewRequest(http.MethodGet, "/settings/downloads", nil))
	if !strings.Contains(page.Body.String(), "not in qBittorrent now") || !strings.Contains(page.Body.String(), "Remove this rule") {
		t.Errorf("page should flag the vanished label and offer to remove its rule:\n%s", page.Body.String())
	}

	// Removing it is the operator's call.
	form := url.Values{}
	for _, row := range data.Labels {
		i := strconv.Itoa(row.Index)
		form.Set("label_"+i, row.Label)
		form.Set("action_"+i, row.Action)
		form.Set("imported_by_"+i, row.ImportedBy)
		if row.Label == "movies" {
			form.Set("remove_"+i, "on")
		}
	}
	save(form)
	if got := srv.currentConfig().Torrents.Labels; len(got) != 1 || got[0].Label != "transfer" {
		t.Errorf("rules after removing movies = %+v", got)
	}
}

// TestDownloadsSettings_QbittorrentDownStillShowsSavedRules: with qBittorrent
// unreachable the page still works, showing the saved rules and why the
// live list is missing.
func TestDownloadsSettings_QbittorrentDownStillShowsSavedRules(t *testing.T) {
	srv, qb := newDownloadsSettingsServer(t)
	if err := srv.updateTorrents(func(c *config.TorrentsConfig) {
		c.Labels = []config.LabelRule{{Label: "movies", Action: config.LabelMove, ImportedBy: "radarr"}}
	}); err != nil {
		t.Fatal(err)
	}
	qb.Close()

	data := srv.downloadsSettingsData()
	if !strings.Contains(data.LabelsError, "Couldn't read qBittorrent's labels") || len(data.Labels) != 1 || data.Labels[0].Label != "movies" {
		t.Fatalf("data = %+v, want the saved rule shown with the error", data)
	}

	if err := srv.connStore.Delete("qbittorrent"); err != nil {
		t.Fatal(err)
	}
	if data := srv.downloadsSettingsData(); !strings.Contains(data.LabelsError, "isn't connected") {
		t.Errorf("LabelsError = %q, want qBittorrent named as not connected", data.LabelsError)
	}
}

func TestDownloadsSettings_Moving(t *testing.T) {
	srv, qb := newDownloadsSettingsServer(t)
	qb.categories["seed"] = ""

	page := httptest.NewRecorder()
	srv.handleDownloadsSettingsPage(page, httptest.NewRequest(http.MethodGet, "/settings/downloads", nil))
	if !strings.Contains(page.Body.String(), `value="3"`) || !strings.Contains(page.Body.String(), `<option value="seed">`) {
		t.Fatalf("page should show the defaults and offer qBittorrent's categories:\n%s", page.Body.String())
	}

	for _, bad := range []url.Values{
		{"settle_days": {"0"}, "seed_label": {"seed"}},
		{"settle_days": {"two"}, "seed_label": {"seed"}},
		{"settle_days": {"3"}, "seed_label": {"  "}},
	} {
		body := serveForm(srv.handleDownloadsMovingSave, "/settings/downloads/moving", bad).Body.String()
		if !strings.Contains(body, "alert-error") {
			t.Errorf("%v should be refused:\n%s", bad, body)
		}
	}

	body := serveForm(srv.handleDownloadsMovingSave, "/settings/downloads/moving", url.Values{
		"settle_days": {"7"}, "seed_label": {" archive "}, "allow_disabling_auto_tmm": {"on"},
	}).Body.String()
	if !strings.Contains(body, "Moving settings saved.") {
		t.Fatalf("save:\n%s", body)
	}
	got := srv.currentConfig().Torrents
	if got.SettleDays != 7 || got.SeedLabel != "archive" || !got.AllowDisablingAutoTMM {
		t.Errorf("saved = %+v", got)
	}

	// A seed label clashing with an ignore rule is refused by validation.
	if err := srv.updateTorrents(func(c *config.TorrentsConfig) {
		c.Labels = []config.LabelRule{{Label: "keep", Action: config.LabelIgnore}}
	}); err != nil {
		t.Fatal(err)
	}
	body = serveForm(srv.handleDownloadsMovingSave, "/settings/downloads/moving", url.Values{"settle_days": {"7"}, "seed_label": {"keep"}}).Body.String()
	if !strings.Contains(body, "seed label") || srv.currentConfig().Torrents.SeedLabel != "archive" {
		t.Errorf("a seed label that's also ignored should be refused:\n%s", body)
	}
}

// TestDownloadsSettings_OnlyWithQbittorrent: the settings subnav offers
// Downloads only once qBittorrent is connected.
func TestDownloadsSettings_OnlyWithQbittorrent(t *testing.T) {
	srv := newSettingsTestServer(t)
	page := func() string {
		rec := httptest.NewRecorder()
		srv.handleConnectionsPage(rec, httptest.NewRequest(http.MethodGet, "/settings/connections", nil))
		return rec.Body.String()
	}
	if strings.Contains(page(), `href="/settings/downloads"`) {
		t.Fatal("no qBittorrent, no Downloads settings")
	}
	if err := srv.connStore.Set("qbittorrent", secrets.Connection{URL: "http://qbittorrent:8080"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(page(), `href="/settings/downloads"`) {
		t.Fatal("with qBittorrent connected, Downloads settings should be offered")
	}
}

// TestNav_DownloadsOnlyInstallHidesLibraryPages: an install with
// qBittorrent and no library tier shows no library pages - even with
// Radarr connected, since download tiering asks it about imports - and a
// library tier brings them back.
func TestNav_DownloadsOnlyInstallHidesLibraryPages(t *testing.T) {
	srv := newSettingsTestServer(t)
	if nav := srv.navState(); !nav.Library || nav.Downloads {
		t.Fatalf("fresh install nav = %+v, want the library pages as ever", nav)
	}
	if err := srv.connStore.Set("qbittorrent", secrets.Connection{URL: "http://qbittorrent:8080"}); err != nil {
		t.Fatal(err)
	}
	if nav := srv.navState(); nav.Library || !nav.Downloads {
		t.Fatalf("downloads-only nav = %+v, want no library pages", nav)
	}
	rec := httptest.NewRecorder()
	srv.handleConnectionsPage(rec, httptest.NewRequest(http.MethodGet, "/settings/connections", nil))
	if strings.Contains(rec.Body.String(), `href="/plan"`) {
		t.Error("a downloads-only install shouldn't link to the library Plan page")
	}
	if err := srv.connStore.Set("radarr", secrets.Connection{URL: "http://radarr:7878", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if nav := srv.navState(); nav.Library {
		t.Fatalf("nav with Radarr connected for imports = %+v, want still no library pages", nav)
	}
	srv.cfg.Tiers = []model.Tier{{Name: "hot", Role: model.RoleHot, Paths: []string{"/hot"}, Media: []model.MediaType{model.Movie}}}
	if nav := srv.navState(); !nav.Library || !nav.Downloads {
		t.Fatalf("nav with a library tier = %+v, want both", nav)
	}
}

// TestDownloadsSettings_SeedingRules: defaults and overrides save, a pasted
// announce URL is cut down to its host - its passkey never reaches the
// config - and an override for a tracker no download uses stays until
// removed.
func TestDownloadsSettings_SeedingRules(t *testing.T) {
	srv, qb := newDownloadsSettingsServer(t)
	qb.add(t, fakeTorrent{Hash: "a1", Name: "A", SavePath: "/downloads", Tracker: "https://live.example.com/announce/KEY", Progress: 0.5})
	qb.trackers["a1"] = []string{"tracker.example.org"}

	data := srv.downloadsSettingsData()
	if data.DefaultSeedDays != 0 || data.GraceDays != 7 || strings.Join(data.TrackerHosts, ",") != "live.example.com" || data.TrackersScanned != "" {
		t.Fatalf("data = %+v, want the defaults and the live tracker offered before any scan", data)
	}

	body := serveForm(srv.handleDownloadsSeedingSave, "/settings/downloads/seeding", url.Values{"min_seed_days": {"14"}, "grace_days": {"0"}}).Body.String()
	if !strings.Contains(body, "Seeding defaults saved.") {
		t.Fatalf("save defaults:\n%s", body)
	}
	if got := srv.currentConfig().Torrents.Seeding; got.MinSeedDays != 14 || got.Grace() != 0 {
		t.Errorf("seeding = %+v, want 14 days and an explicit zero grace", got)
	}
	if body := serveForm(srv.handleDownloadsSeedingSave, "/settings/downloads/seeding", url.Values{"min_seed_days": {"-1"}, "grace_days": {"3"}}).Body.String(); !strings.Contains(body, "0 or more") {
		t.Errorf("a negative default should be refused:\n%s", body)
	}

	save := func(host, days string) string {
		t.Helper()
		return serveForm(srv.handleDownloadsTrackerSave, "/settings/downloads/trackers", url.Values{"host": {host}, "min_seed_days": {days}}).Body.String()
	}
	if body := save("https://Tracker.Example.org:443/announce/SECRETKEY", "21"); !strings.Contains(body, "Override for tracker.example.org saved.") {
		t.Fatalf("save override:\n%s", body)
	}
	save("tracker.example.org", "30") // the same host again updates it
	save("gone.example.net", "0")
	if body := save("", "5"); !strings.Contains(body, "needs a tracker host") {
		t.Errorf("an empty host should be refused:\n%s", body)
	}
	overrides := srv.currentConfig().Torrents.Seeding.Overrides
	if len(overrides) != 2 || overrides[0] != (config.TrackerOverride{Host: "tracker.example.org", MinSeedDays: 30}) {
		t.Fatalf("overrides = %+v", overrides)
	}
	raw, err := os.ReadFile(srv.cfgPath)
	if err != nil || strings.Contains(string(raw), "SECRETKEY") || strings.Contains(string(raw), "announce") {
		t.Fatalf("the config file holds more than the host:\n%s", raw)
	}

	// Scan, and the overrides show which trackers are in use.
	if body := serveForm(srv.handleDownloadsTrackerScan, "/settings/downloads/trackers/scan", nil).Body.String(); !strings.Contains(body, "1 download(s) looked up") {
		t.Fatalf("scan:\n%s", body)
	}
	data = srv.downloadsSettingsData()
	byHost := map[string]trackerOverrideView{}
	for _, o := range data.Overrides {
		byHost[o.Host] = o
	}
	if byHost["tracker.example.org"].Downloads != 1 || byHost["gone.example.net"].Downloads != 0 || data.TrackersScanned == "" {
		t.Errorf("overrides = %+v, scanned %q", data.Overrides, data.TrackersScanned)
	}
	page := httptest.NewRecorder()
	srv.handleDownloadsSettingsPage(page, httptest.NewRequest(http.MethodGet, "/settings/downloads", nil))
	if !strings.Contains(page.Body.String(), "none right now") || !strings.Contains(page.Body.String(), `<option value="tracker.example.org">`) {
		t.Errorf("page should flag the unused override and offer scanned trackers:\n%s", page.Body.String())
	}

	body = serveForm(srv.handleDownloadsTrackerDelete, "/settings/downloads/trackers/delete", url.Values{"host": {"gone.example.net"}}).Body.String()
	if !strings.Contains(body, "removed") || len(srv.currentConfig().Torrents.Seeding.Overrides) != 1 {
		t.Errorf("delete:\n%s", body)
	}

	// A scan already running is refused, not doubled up.
	srv.scanTrackersMu.Lock()
	body = serveForm(srv.handleDownloadsTrackerScan, "/settings/downloads/trackers/scan", nil).Body.String()
	srv.scanTrackersMu.Unlock()
	if !strings.Contains(body, "already in progress") {
		t.Errorf("overlapping scan:\n%s", body)
	}
}

func TestScheduledTrackerScan(t *testing.T) {
	srv, qb := newDownloadsSettingsServer(t)
	apprise := newFakeApprise(t)
	if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
		t.Fatal(err)
	}
	qb.add(t, fakeTorrent{Hash: "a1", Name: "A", SavePath: "/downloads", Progress: 0.5})
	qb.trackers["a1"] = []string{"tracker.example.org"}

	srv.runScheduledScanTrackers(time.Now())
	if got := apprise.notifications(); len(got) != 1 || !strings.Contains(got[0]["body"], "1 download(s) looked up, 1 tracker(s) known") {
		t.Fatalf("notifications = %+v", got)
	}

	qb.Close()
	srv.runScheduledScanTrackers(time.Now())
	if got := apprise.notifications(); len(got) != 2 || got[1]["type"] != "failure" {
		t.Errorf("a failed scan should notify: %+v", got)
	}

	rec := serveForm(srv.handleScanTrackersNow, "/settings/scheduler/scan_trackers/run", nil)
	if !strings.Contains(rec.Body.String(), "listing qBittorrent") {
		t.Errorf("Scan now with qBittorrent down:\n%s", rec.Body.String())
	}
}

func TestDownloadsSettings_Removal(t *testing.T) {
	srv, _ := newDownloadsSettingsServer(t)
	data := srv.downloadsSettingsData()
	if data.RemovalMode != "off" || data.MaxPerRun != 25 {
		t.Fatalf("removal defaults = %q, %d; want off and 25", data.RemovalMode, data.MaxPerRun)
	}
	body := serveForm(srv.handleDownloadsRemovalSave, "/settings/downloads/removal", url.Values{"mode": {"report"}, "max_per_run": {"10"}}).Body.String()
	if !strings.Contains(body, "Removal settings saved.") {
		t.Fatalf("save:\n%s", body)
	}
	if got := srv.currentConfig().Torrents.Removal; got.Mode != config.RemovalReport || got.MaxPerRun != 10 {
		t.Errorf("removal = %+v", got)
	}
	for _, bad := range []url.Values{
		{"mode": {"remove"}, "max_per_run": {"0"}},
		{"mode": {"purge"}, "max_per_run": {"5"}},
	} {
		if body := serveForm(srv.handleDownloadsRemovalSave, "/settings/downloads/removal", bad).Body.String(); !strings.Contains(body, "alert-error") {
			t.Errorf("%v should be refused:\n%s", bad, body)
		}
	}
	if got := srv.currentConfig().Torrents.Removal.Mode; got != config.RemovalReport {
		t.Errorf("a refused save changed the mode to %q", got)
	}
}
