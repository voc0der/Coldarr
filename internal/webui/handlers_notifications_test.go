package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/secrets"
)

// TestNotificationsSave: the Apprise URL can work as a bearer credential,
// so it goes to the encrypted store and is never rendered back; the
// formatting flags and tag go to coldarr.yaml.
func TestNotificationsSave(t *testing.T) {
	srv := newSettingsTestServer(t)
	page := httptest.NewRecorder()
	srv.handleNotificationsPage(page, httptest.NewRequest(http.MethodGet, "/settings/notifications", nil))
	if !strings.Contains(page.Body.String(), `placeholder="https://notify.example.com/notify/apprise"`) {
		t.Fatalf("with nothing saved the page should prompt for a URL:\n%s", page.Body.String())
	}

	save := func(form url.Values) string {
		t.Helper()
		return serveForm(srv.handleNotificationsSave, "/settings/notifications", form).Body.String()
	}

	if body := save(url.Values{"apprise_url": {"mailto:me@example.com"}}); !strings.Contains(body, "must be a valid http:// or https:// URL") {
		t.Fatalf("a non-HTTP Apprise URL should be refused:\n%s", body)
	}
	if _, ok := srv.connStore.Get("apprise"); ok {
		t.Fatal("a refused URL must not be stored")
	}

	const appriseURL = "https://apprise.example.com/notify/secret-key"
	body := save(url.Values{"apprise_url": {appriseURL}, "verbose": {"on"}, "markdown": {"on"}, "tag": {" mobile "}})
	if !strings.Contains(body, "Notification settings saved.") {
		t.Fatalf("save did not report success:\n%s", body)
	}
	if strings.Contains(body, "secret-key") {
		t.Error("the saved Apprise URL must not be rendered back into the page")
	}
	if got, _ := srv.connStore.Get("apprise"); got.URL != appriseURL {
		t.Fatalf("stored apprise = %+v, want %s", got, appriseURL)
	}
	if got := srv.currentConfig().Notifications; !got.Verbose || !got.Markdown || got.Tag != "mobile" {
		t.Fatalf("notifications config = %+v, want verbose, markdown, tag mobile", got)
	}
	if raw, err := os.ReadFile(srv.cfgPath); err != nil || strings.Contains(string(raw), "secret-key") {
		t.Fatalf("coldarr.yaml must never hold the Apprise URL (err %v):\n%s", err, raw)
	}

	// A blank URL keeps the saved one; unticked boxes switch the flags off.
	save(url.Values{"tag": {"mobile"}})
	if got, _ := srv.connStore.Get("apprise"); got.URL != appriseURL {
		t.Fatalf("stored apprise = %+v, want the saved URL kept", got)
	}
	if got := srv.currentConfig().Notifications; got.Verbose || got.Markdown {
		t.Fatalf("notifications config = %+v, want verbose and markdown off", got)
	}

	rec := serveForm(srv.handleNotificationsDelete, "/settings/notifications/delete", nil)
	if !strings.Contains(rec.Body.String(), "Apprise URL removed.") {
		t.Fatalf("remove did not report success:\n%s", rec.Body.String())
	}
	if _, ok := srv.connStore.Get("apprise"); ok {
		t.Fatal("expected the Apprise URL removed")
	}
}

func TestNotificationsSave_ReportsWhatCouldNotBeSaved(t *testing.T) {
	srv := newSettingsTestServer(t)
	if err := os.Mkdir(srv.cfgPath+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	rec := serveForm(srv.handleNotificationsSave, "/settings/notifications", url.Values{"verbose": {"on"}})
	if body := rec.Body.String(); !strings.Contains(body, "alert-error") || strings.Contains(body, "settings saved") {
		t.Fatalf("a failed config write should be reported:\n%s", body)
	}
	if srv.currentConfig().Notifications.Verbose {
		t.Error("a failed save must leave the live config as it was")
	}
}

// TestNotificationsTest sends a real test notification. Whatever is typed
// into the form - URL, tag, Markdown box - is used before it's saved, and
// with nothing typed it falls back to what's saved.
func TestNotificationsTest(t *testing.T) {
	apprise := newFakeApprise(t)
	srv := newSettingsTestServer(t)
	test := func(form url.Values) string {
		t.Helper()
		return serveForm(srv.handleNotificationsTest, "/settings/notifications/test", form).Body.String()
	}

	if body := test(nil); !strings.Contains(body, "Enter an Apprise URL (or save one first) to test") {
		t.Fatalf("with nothing typed or saved there's nothing to test:\n%s", body)
	}
	if body := test(url.Values{"apprise_url": {"apprise.example.com"}}); !strings.Contains(body, "must be a valid http:// or https:// URL") {
		t.Fatalf("an invalid URL should be refused before sending:\n%s", body)
	}

	if body := test(url.Values{"apprise_url": {apprise.URL}, "tag": {"desk"}, "markdown": {"on"}}); !strings.Contains(body, "Sent") {
		t.Fatalf("expected the test sent:\n%s", body)
	}

	if err := srv.connStore.Set("apprise", secrets.Connection{URL: apprise.URL}); err != nil {
		t.Fatal(err)
	}
	if err := srv.updateNotifications(false, false, "saved-tag"); err != nil {
		t.Fatal(err)
	}
	if body := test(nil); !strings.Contains(body, "Sent") {
		t.Fatalf("expected the saved URL used:\n%s", body)
	}

	got := apprise.notifications()
	if len(got) != 2 {
		t.Fatalf("notifications received = %d, want 2", len(got))
	}
	if got[0]["tag"] != "desk" || got[0]["format"] != "markdown" {
		t.Errorf("first test = %v, want the typed tag and Markdown", got[0])
	}
	if got[1]["tag"] != "saved-tag" || got[1]["format"] != "" {
		t.Errorf("second test = %v, want the saved tag, plain text", got[1])
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no route for tag", http.StatusFailedDependency)
	}))
	t.Cleanup(failing.Close)
	if body := test(url.Values{"apprise_url": {failing.URL}}); !strings.Contains(body, "Failed:") || !strings.Contains(body, "424") {
		t.Fatalf("expected the endpoint's failure shown:\n%s", body)
	}
}
