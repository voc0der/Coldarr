package webui

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/config"
)

// serveForm calls handler with form POSTed to target, the way a settings
// page submits, and returns the response. pathValues are name/value pairs
// for the route's {wildcards}.
func serveForm(handler http.HandlerFunc, target string, form url.Values, pathValues ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(pathValues); i += 2 {
		req.SetPathValue(pathValues[i], pathValues[i+1])
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func oidcSettingsForm(edits map[string]string) url.Values {
	form := url.Values{
		"enabled":       {"on"},
		"issuer_url":    {"https://auth.example.com"},
		"client_id":     {"coldarr"},
		"client_secret": {"s3cret"},
	}
	for k, v := range edits {
		if v == "" {
			form.Del(k)
		} else {
			form.Set(k, v)
		}
	}
	return form
}

// TestAuthSave_StoresSettingsAndSecretSeparately: the OIDC settings go to
// coldarr.yaml, but the client secret goes to the encrypted connection
// store - it must never land in the plain config file.
func TestAuthSave_StoresSettingsAndSecretSeparately(t *testing.T) {
	srv := newAuthTestServer(t, false)

	rec := serveForm(srv.handleAuthSave, "/settings/auth", oidcSettingsForm(map[string]string{
		"client_secret_post": "on",
		"auto_login":         "on",
		"redirect_url":       "https://coldarr.example.com/auth/callback",
	}))
	if !strings.Contains(rec.Body.String(), "OIDC auth settings saved.") {
		t.Fatalf("save did not report success:\n%s", rec.Body.String())
	}

	got := srv.currentConfig().Auth.OIDC
	want := config.OIDCAuthConfig{
		Enabled: true, IssuerURL: "https://auth.example.com", ClientID: "coldarr",
		RedirectURL:   "https://coldarr.example.com/auth/callback",
		RequiredGroup: "coldarr", GroupsClaim: "groups", // blank fields take the defaults
		TokenAuthMethod: oidcTokenAuthClientPost, AutoLogin: true,
	}
	if got != want {
		t.Fatalf("live OIDC config = %+v, want %+v", got, want)
	}

	raw, err := os.ReadFile(srv.cfgPath)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}
	if !strings.Contains(string(raw), "https://auth.example.com") || strings.Contains(string(raw), "s3cret") {
		t.Fatalf("coldarr.yaml should hold the settings but never the secret:\n%s", raw)
	}
	if stored, ok := srv.connStore.Get(oidcSecretApp); !ok || stored.URL != "coldarr" || stored.APIKey != "s3cret" {
		t.Fatalf("stored OIDC secret = %+v, want client coldarr with its secret", stored)
	}
}

// TestAuthSave_BlankSecretKeepsTheStoredOne: the secret is never sent back
// to the browser, so saving the form again with the field left blank must
// keep it rather than wipe it.
func TestAuthSave_BlankSecretKeepsTheStoredOne(t *testing.T) {
	srv := newAuthTestServer(t, false)
	if err := srv.connStore.Set(oidcSecretApp, storedOIDCSecret("old-client", "kept-secret")); err != nil {
		t.Fatal(err)
	}

	rec := serveForm(srv.handleAuthSave, "/settings/auth", oidcSettingsForm(map[string]string{"client_id": "new-client", "client_secret": ""}))
	if !strings.Contains(rec.Body.String(), "OIDC auth settings saved.") {
		t.Fatalf("save did not report success:\n%s", rec.Body.String())
	}
	if stored, _ := srv.connStore.Get(oidcSecretApp); stored.URL != "new-client" || stored.APIKey != "kept-secret" {
		t.Fatalf("stored OIDC secret = %+v, want the new client ID with the kept secret", stored)
	}
	if got := srv.currentConfig().Auth.OIDC.TokenAuthMethod; got != oidcTokenAuthClientBasic {
		t.Errorf("TokenAuthMethod = %q with client_secret_post unticked, want %q", got, oidcTokenAuthClientBasic)
	}
}

// TestAuthSave_IncompleteProviderIsRejectedAndKeepsTheForm: switching OIDC
// on with something missing would lock everyone out of the GUI, so it's
// refused - with what was typed still in the form - and nothing is saved.
func TestAuthSave_IncompleteProviderIsRejectedAndKeepsTheForm(t *testing.T) {
	srv := newAuthTestServer(t, false)

	rec := serveForm(srv.handleAuthSave, "/settings/auth", oidcSettingsForm(map[string]string{"issuer_url": "", "client_id": "typed-client"}))
	body := rec.Body.String()
	if !strings.Contains(body, "issuer URL is required") {
		t.Fatalf("expected the validation error:\n%s", body)
	}
	if !strings.Contains(body, `value="typed-client"`) {
		t.Errorf("the rejected form should keep what was typed:\n%s", body)
	}
	if srv.currentConfig().Auth.OIDC.Enabled {
		t.Error("an invalid OIDC config must not be switched on")
	}
	if _, err := os.Stat(srv.cfgPath); !os.IsNotExist(err) {
		t.Errorf("nothing should be written for a rejected save, stat err = %v", err)
	}
	if _, ok := srv.connStore.Get(oidcSecretApp); ok {
		t.Error("a rejected save must not store the secret")
	}
}

// TestAuthSave_ClearingTheClientRemovesItsStoredEntry: disabling OIDC and
// clearing the client ID, with no secret on file, leaves nothing behind.
func TestAuthSave_ClearingTheClientRemovesItsStoredEntry(t *testing.T) {
	srv := newAuthTestServer(t, false)
	if err := srv.connStore.Set(oidcSecretApp, storedOIDCSecret("old-client", "")); err != nil {
		t.Fatal(err)
	}

	rec := serveForm(srv.handleAuthSave, "/settings/auth", url.Values{})
	if !strings.Contains(rec.Body.String(), "OIDC auth settings saved.") {
		t.Fatalf("save did not report success:\n%s", rec.Body.String())
	}
	if _, ok := srv.connStore.Get(oidcSecretApp); ok {
		t.Fatal("expected the empty OIDC entry to be removed")
	}
}

// TestAuthPage_EnvLockedSettingsAreReadOnly: OIDC set through COLDARR_OIDC_*
// env vars always wins, so the page shows it read-only and a save is
// refused rather than writing settings that would silently never apply.
func TestAuthPage_EnvLockedSettingsAreReadOnly(t *testing.T) {
	t.Setenv("COLDARR_OIDC_ISSUER_URL", "https://env.example.com")
	srv := newAuthTestServer(t, false)

	page := httptest.NewRecorder()
	srv.handleAuthPage(page, httptest.NewRequest(http.MethodGet, "/settings/auth", nil))
	if body := page.Body.String(); !strings.Contains(body, "environment variables") || strings.Contains(body, `action="/settings/auth"`) {
		t.Fatalf("an env-locked auth page should be read-only:\n%s", body)
	}

	rec := serveForm(srv.handleAuthSave, "/settings/auth", oidcSettingsForm(nil))
	if !strings.Contains(rec.Body.String(), "cannot be edited here") {
		t.Fatalf("expected the save to be refused:\n%s", rec.Body.String())
	}
	if srv.currentConfig().Auth.OIDC.Enabled {
		t.Error("an env-locked save must not change the config")
	}
	if _, err := os.Stat(srv.cfgPath); !os.IsNotExist(err) {
		t.Errorf("an env-locked save must not write coldarr.yaml, stat err = %v", err)
	}
}

func TestAuthSave_ReportsWhatCouldNotBeSaved(t *testing.T) {
	t.Run("config file", func(t *testing.T) {
		srv := newAuthTestServer(t, false)
		if err := os.Mkdir(srv.cfgPath+".tmp", 0o750); err != nil {
			t.Fatal(err)
		}
		rec := serveForm(srv.handleAuthSave, "/settings/auth", oidcSettingsForm(nil))
		if body := rec.Body.String(); !strings.Contains(body, "alert-error") || strings.Contains(body, "settings saved") {
			t.Fatalf("expected the failed save reported:\n%s", body)
		}
		if srv.currentConfig().Auth.OIDC.Enabled {
			t.Error("a failed save must leave the live config as it was")
		}
		if _, ok := srv.connStore.Get(oidcSecretApp); ok {
			t.Error("a failed save must not store the secret on its own")
		}
	})

	t.Run("secret store", func(t *testing.T) {
		srv := newAuthTestServer(t, false)
		if err := os.Mkdir(filepath.Join(filepath.Dir(srv.cfgPath), "connections.enc.json.tmp"), 0o750); err != nil {
			t.Fatal(err)
		}
		rec := serveForm(srv.handleAuthSave, "/settings/auth", oidcSettingsForm(nil))
		if !strings.Contains(rec.Body.String(), "auth settings saved, but storing OIDC secret failed") {
			t.Fatalf("expected the half-saved state spelled out:\n%s", rec.Body.String())
		}
	})
}
