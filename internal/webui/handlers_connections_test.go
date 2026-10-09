package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vocoder/coldarr/internal/secrets"
)

// newSettingsTestServer is a Server with nothing configured yet, as on a
// fresh install - all the settings pages need.
func newSettingsTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv(passwordEnvVar, "pw")
	return newAuthTestServer(t, false)
}

func TestConnectionSave(t *testing.T) {
	srv := newSettingsTestServer(t)
	save := func(app string, form url.Values) string {
		t.Helper()
		return serveForm(srv.handleConnectionSave, "/settings/connections/"+app, form, "app", app).Body.String()
	}

	if body := save("radarr", url.Values{"api_key": {"k"}}); !strings.Contains(body, "URL is required") {
		t.Fatalf("saving without a URL should be refused:\n%s", body)
	}
	if body := save("radarr", url.Values{"url": {"http://radarr:7878"}}); !strings.Contains(body, "API key is required") {
		t.Fatalf("a first save without an API key should be refused:\n%s", body)
	}
	if _, ok := srv.connStore.Get("radarr"); ok {
		t.Fatal("a refused save must not store anything")
	}

	// Radarr is always enabled once configured, whatever the form says.
	body := save("radarr", url.Values{"url": {" http://radarr:7878 "}, "api_key": {" key-1 "}})
	if !strings.Contains(body, "radarr connection saved.") {
		t.Fatalf("save did not report success:\n%s", body)
	}
	if got, _ := srv.connStore.Get("radarr"); got != (secrets.Connection{URL: "http://radarr:7878", APIKey: "key-1", Enabled: true}) {
		t.Fatalf("stored radarr = %+v, want the trimmed URL and key, enabled", got)
	}
	// The key is never sent back to the browser - the form just says it's set.
	if strings.Contains(body, "key-1") || !strings.Contains(body, "(unchanged - leave blank to keep it)") {
		t.Error("the saved API key must not be rendered back into the page")
	}

	// A blank key on a later save keeps the stored one.
	save("radarr", url.Values{"url": {"http://radarr.lan:7878"}})
	if got, _ := srv.connStore.Get("radarr"); got.URL != "http://radarr.lan:7878" || got.APIKey != "key-1" {
		t.Fatalf("stored radarr = %+v, want the new URL with the kept key", got)
	}

	// Jellyfin is the one connection that can be saved switched off.
	save("jellyfin", url.Values{"url": {"http://jellyfin:8096"}, "api_key": {"jf"}})
	if got, _ := srv.connStore.Get("jellyfin"); got.Enabled {
		t.Fatalf("stored jellyfin = %+v, want it saved disabled", got)
	}
	save("jellyfin", url.Values{"url": {"http://jellyfin:8096"}, "enabled": {"on"}})
	if got, _ := srv.connStore.Get("jellyfin"); !got.Enabled || got.APIKey != "jf" {
		t.Fatalf("stored jellyfin = %+v, want it enabled with its key kept", got)
	}
}

func TestConnectionTest(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	radarr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("X-Api-Key"))
		mu.Unlock()
		if r.Header.Get("X-Api-Key") == "wrong" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"version": "5.2.0"}`))
	}))
	t.Cleanup(radarr.Close)

	srv := newSettingsTestServer(t)
	test := func(form url.Values) string {
		t.Helper()
		return serveForm(srv.handleConnectionTest, "/settings/connections/radarr/test", form, "app", "radarr").Body.String()
	}

	if body := test(nil); !strings.Contains(body, "URL and API key are required to test") {
		t.Fatalf("with nothing typed or saved there's nothing to test:\n%s", body)
	}

	// What's typed is tested before it's saved.
	if body := test(url.Values{"url": {radarr.URL}, "api_key": {"typed"}}); !strings.Contains(body, "Connected - version 5.2.0") {
		t.Fatalf("expected a successful test:\n%s", body)
	}

	// Already configured: testing without retyping the key uses the saved one.
	if err := srv.connStore.Set("radarr", secrets.Connection{URL: radarr.URL, APIKey: "saved"}); err != nil {
		t.Fatal(err)
	}
	if body := test(url.Values{"url": {radarr.URL}}); !strings.Contains(body, "Connected") {
		t.Fatalf("expected the saved key to be used:\n%s", body)
	}

	if body := test(url.Values{"url": {radarr.URL}, "api_key": {"wrong"}}); !strings.Contains(body, "Failed:") || !strings.Contains(body, "401") {
		t.Fatalf("expected the failure shown:\n%s", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"typed", "saved", "wrong"}; strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys sent = %v, want %v", keys, want)
	}
}

// TestConnectionExternalURL_SavesOnItsOwn: the external URL only builds
// browser links, so it can be saved independently - even before the
// connection itself is set up - and shows back on the form.
func TestConnectionExternalURL_SavesOnItsOwn(t *testing.T) {
	srv := newSettingsTestServer(t)

	rec := serveForm(srv.handleConnectionExternalURLSave, "/settings/connections/sonarr/external-url",
		url.Values{"external_url": {" https://sonarr.example.com "}}, "app", "sonarr")
	body := rec.Body.String()
	if !strings.Contains(body, "sonarr external URL saved.") || !strings.Contains(body, `value="https://sonarr.example.com"`) {
		t.Fatalf("expected the external URL saved and shown:\n%s", body)
	}
	if got, _ := srv.connStore.Get("sonarr"); got.ExternalURL != "https://sonarr.example.com" {
		t.Fatalf("stored sonarr = %+v, want the external URL", got)
	}
	if _, source := srv.connStore.Effective("sonarr"); source != secrets.SourceNone {
		t.Errorf("an external URL alone must not make Sonarr count as configured, source = %q", source)
	}

	serveForm(srv.handleConnectionExternalURLSave, "/settings/connections/sonarr/external-url", url.Values{"external_url": {""}}, "app", "sonarr")
	if got, _ := srv.connStore.Get("sonarr"); got.ExternalURL != "" {
		t.Fatalf("stored sonarr = %+v, want the external URL cleared", got)
	}
}

func TestConnectionDelete(t *testing.T) {
	srv := newSettingsTestServer(t)
	if err := srv.connStore.Set("sonarr", secrets.Connection{URL: "http://sonarr:8989", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	rec := serveForm(srv.handleConnectionDelete, "/settings/connections/sonarr/delete", nil, "app", "sonarr")
	if !strings.Contains(rec.Body.String(), "sonarr connection removed.") {
		t.Fatalf("delete did not report success:\n%s", rec.Body.String())
	}
	if _, ok := srv.connStore.Get("sonarr"); ok {
		t.Fatal("expected the stored connection removed")
	}
}

func TestConnectionHandlers_RejectUnknownApps(t *testing.T) {
	srv := newSettingsTestServer(t)
	for name, handler := range map[string]http.HandlerFunc{
		"save":         srv.handleConnectionSave,
		"test":         srv.handleConnectionTest,
		"external-url": srv.handleConnectionExternalURLSave,
		"delete":       srv.handleConnectionDelete,
	} {
		rec := serveForm(handler, "/settings/connections/lidarr", url.Values{"url": {"http://lidarr:8686"}, "api_key": {"k"}}, "app", "lidarr")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s for an unknown app = %d, want 404", name, rec.Code)
		}
	}
	if _, ok := srv.connStore.Get("lidarr"); ok {
		t.Error("nothing should be stored for an unknown app")
	}
}

// TestConnectionsPage_EnvConfiguredAppIsReadOnly: a connection set by env
// vars always wins over the stored one, so its form is replaced by a note
// naming the variables - and the env-supplied key is never rendered.
func TestConnectionsPage_EnvConfiguredAppIsReadOnly(t *testing.T) {
	t.Setenv("RADARR_URL", "http://radarr-env:7878")
	t.Setenv("RADARR_API_KEY", "env-key")
	srv := newSettingsTestServer(t)

	rec := httptest.NewRecorder()
	srv.handleConnectionsPage(rec, httptest.NewRequest(http.MethodGet, "/settings/connections", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "<code>RADARR_URL</code>") || !strings.Contains(body, "http://radarr-env:7878") {
		t.Errorf("expected Radarr shown as set by env vars:\n%s", body)
	}
	if strings.Contains(body, `action="/settings/connections/radarr"`) {
		t.Error("an env-configured connection must not offer an edit form")
	}
	if !strings.Contains(body, `action="/settings/connections/sonarr"`) || !strings.Contains(body, `placeholder="http://sonarr:8989"`) {
		t.Error("an unconfigured app should offer its form with the default-port placeholder")
	}
	if strings.Contains(body, "env-key") {
		t.Error("the env-supplied API key must never be rendered")
	}
}

func TestConnectionHandlers_ReportStoreFailures(t *testing.T) {
	srv := newSettingsTestServer(t)
	if err := os.Mkdir(filepath.Join(filepath.Dir(srv.cfgPath), "connections.enc.json.tmp"), 0o750); err != nil {
		t.Fatal(err)
	}

	for name, handler := range map[string]http.HandlerFunc{
		"save":         srv.handleConnectionSave,
		"external-url": srv.handleConnectionExternalURLSave,
		"delete":       srv.handleConnectionDelete,
	} {
		rec := serveForm(handler, "/settings/connections/radarr", url.Values{"url": {"http://radarr:7878"}, "api_key": {"k"}, "external_url": {"https://radarr.example.com"}}, "app", "radarr")
		if body := rec.Body.String(); !strings.Contains(body, "alert-error") || strings.Contains(body, "alert-ok") {
			t.Errorf("%s: a failed store write should be reported, not shown as saved:\n%s", name, body)
		}
	}
}

// TestConnectionSave_KeepsTheExternalURL: the External URL has its own
// form, so saving a connection's URL or key must leave it alone. Every
// connection save used to wipe it.
func TestConnectionSave_KeepsTheExternalURL(t *testing.T) {
	srv := newSettingsTestServer(t)
	serveForm(srv.handleConnectionExternalURLSave, "/settings/connections/radarr/external-url",
		url.Values{"external_url": {"https://radarr.example.com"}}, "app", "radarr")

	for _, form := range []url.Values{
		{"url": {"http://radarr:7878"}, "api_key": {"key-1"}}, // first save, key typed
		{"url": {"http://radarr.lan:7878"}},                   // later save, key kept
	} {
		body := serveForm(srv.handleConnectionSave, "/settings/connections/radarr", form, "app", "radarr").Body.String()
		got, _ := srv.connStore.Get("radarr")
		if got != (secrets.Connection{URL: form.Get("url"), APIKey: "key-1", Enabled: true, ExternalURL: "https://radarr.example.com"}) {
			t.Fatalf("stored radarr after saving %v = %+v, want the External URL kept", form, got)
		}
		if !strings.Contains(body, `id="ext-radarr" name="external_url" placeholder="https://radarr.mydomain.com" value="https://radarr.example.com"`) {
			t.Errorf("the page after saving %v should still show the External URL:\n%s", form, body)
		}
	}
}
