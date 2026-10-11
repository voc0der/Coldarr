package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

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
