package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/secrets"
)

func TestCleanReturnTo(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "", want: "/"},
		{raw: "/plan?page=1", want: "/plan?page=1"},
		{raw: "https://evil.example/plan", want: "/"},
		{raw: "//evil.example/plan", want: "/"},
		{raw: "/\\evil.example/plan", want: "/"},
		{raw: "/auth/login", want: "/"},
		{raw: "/login", want: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := cleanReturnTo(tt.raw); got != tt.want {
				t.Fatalf("cleanReturnTo(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestGroupAllowed(t *testing.T) {
	if !groupAllowed([]string{"coldarr", "media"}, "coldarr") {
		t.Fatal("expected required group to be allowed")
	}
	if groupAllowed([]string{"media"}, "coldarr") {
		t.Fatal("expected missing required group to be denied")
	}
	if !groupAllowed(nil, "") {
		t.Fatal("blank required group should allow authenticated users")
	}
}

func TestClaimStringSlice(t *testing.T) {
	primary := map[string]any{"groups": []any{"coldarr", "media"}}
	fallback := map[string]any{"groups": "fallback"}

	got := claimStringSlice(primary, fallback, "groups")
	if len(got) != 2 || got[0] != "coldarr" || got[1] != "media" {
		t.Fatalf("claimStringSlice array = %#v, want coldarr/media", got)
	}

	got = claimStringSlice(map[string]any{}, map[string]any{"groups": "coldarr, media"}, "groups")
	if len(got) != 2 || got[0] != "coldarr" || got[1] != "media" {
		t.Fatalf("claimStringSlice string fallback = %#v, want coldarr/media", got)
	}
}

func TestEffectiveOIDCConfigEnvOverridesStoredValues(t *testing.T) {
	store, err := secrets.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(oidcSecretApp, storedOIDCSecret("stored-client", "stored-secret")); err != nil {
		t.Fatal(err)
	}

	s := &Server{
		cfg: &config.Config{Auth: config.AuthConfig{OIDC: config.OIDCAuthConfig{
			Enabled:       true,
			IssuerURL:     "https://stored.example",
			ClientID:      "stored-client",
			RequiredGroup: "coldarr",
			GroupsClaim:   "groups",
			AutoLogin:     true,
		}}},
		connStore: store,
	}

	t.Setenv("COLDARR_OIDC_ISSUER_URL", "https://env.example")
	t.Setenv("COLDARR_OIDC_CLIENT_ID", "env-client")
	t.Setenv("COLDARR_OIDC_CLIENT_SECRET", "env-secret")
	t.Setenv("COLDARR_OIDC_REQUIRED_GROUP", "env-group")
	t.Setenv("COLDARR_OIDC_CLIENT_SECRET_POST", "true")
	t.Setenv("COLDARR_OIDC_AUTO_LOGIN", "false")

	got := s.effectiveOIDCConfig()
	if got.IssuerURL != "https://env.example" || got.ClientID != "env-client" || got.ClientSecret != "env-secret" {
		t.Fatalf("env values did not override stored config: %+v", got)
	}
	if got.RequiredGroup != "env-group" {
		t.Fatalf("RequiredGroup = %q, want env-group", got.RequiredGroup)
	}
	if got.TokenAuthMethod != oidcTokenAuthClientPost {
		t.Fatalf("TokenAuthMethod = %q, want %q", got.TokenAuthMethod, oidcTokenAuthClientPost)
	}
	if got.AutoLogin {
		t.Fatal("COLDARR_OIDC_AUTO_LOGIN=false should override the stored AutoLogin=true")
	}
	if !got.EnvLocked {
		t.Fatal("expected EnvLocked when OIDC env vars are set")
	}
}

func TestOIDCOAuth2AuthStyle(t *testing.T) {
	tests := []struct {
		method string
		want   oauth2.AuthStyle
	}{
		{method: oidcTokenAuthAuto, want: oauth2.AuthStyleAutoDetect},
		{method: oidcTokenAuthClientPost, want: oauth2.AuthStyleInParams},
		{method: oidcTokenAuthClientBasic, want: oauth2.AuthStyleInHeader},
		{method: "", want: oauth2.AuthStyleAutoDetect},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			if got := oidcOAuth2AuthStyle(tt.method); got != tt.want {
				t.Fatalf("oidcOAuth2AuthStyle(%q) = %v, want %v", tt.method, got, tt.want)
			}
		})
	}
}

func newAuthTestServer(t *testing.T, oidcEnabled bool) *Server {
	t.Helper()
	dir := t.TempDir()
	connStore, err := secrets.LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("secrets.LoadOrCreate: %v", err)
	}
	cfg := &config.Config{}
	if oidcEnabled {
		cfg.Auth.OIDC = config.OIDCAuthConfig{
			Enabled:       true,
			IssuerURL:     "https://issuer.example",
			ClientID:      "client",
			RequiredGroup: "coldarr",
			GroupsClaim:   "groups",
		}
		if err := connStore.Set(oidcSecretApp, storedOIDCSecret("client", "secret")); err != nil {
			t.Fatalf("connStore.Set: %v", err)
		}
	}
	srv, err := New(filepath.Join(dir, "coldarr.yaml"), cfg, connStore)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestResolvePassword(t *testing.T) {
	t.Run("explicit COLDARR_PASSWORD", func(t *testing.T) {
		t.Setenv(passwordEnvVar, "hunter2")
		pw, generated, err := resolvePassword()
		if err != nil {
			t.Fatalf("resolvePassword: %v", err)
		}
		if pw != "hunter2" || generated {
			t.Fatalf("got (%q, %v), want (\"hunter2\", false)", pw, generated)
		}
	})

	t.Run("COLDARR_PASSWORD_FILE overrides COLDARR_PASSWORD", func(t *testing.T) {
		t.Setenv(passwordEnvVar, "hunter2")
		path := filepath.Join(t.TempDir(), "password")
		if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(passwordFileEnvVar, path)
		pw, generated, err := resolvePassword()
		if err != nil {
			t.Fatalf("resolvePassword: %v", err)
		}
		if pw != "from-file" || generated {
			t.Fatalf("got (%q, %v), want (\"from-file\", false)", pw, generated)
		}
	})

	t.Run("COLDARR_PASSWORD_FILE missing errors", func(t *testing.T) {
		t.Setenv(passwordFileEnvVar, filepath.Join(t.TempDir(), "does-not-exist"))
		if _, _, err := resolvePassword(); err == nil {
			t.Fatal("expected an error for a missing COLDARR_PASSWORD_FILE")
		}
	})

	t.Run("COLDARR_PASSWORD_FILE empty errors", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "password")
		if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(passwordFileEnvVar, path)
		if _, _, err := resolvePassword(); err == nil {
			t.Fatal("expected an error for an empty COLDARR_PASSWORD_FILE")
		}
	})

	t.Run("neither set generates a 64-character password", func(t *testing.T) {
		pw, generated, err := resolvePassword()
		if err != nil {
			t.Fatalf("resolvePassword: %v", err)
		}
		if !generated {
			t.Fatal("expected generated=true when neither env var is set")
		}
		if len(pw) != 64 {
			t.Fatalf("generated password length = %d, want 64", len(pw))
		}
	})
}

func TestPasswordAuth_FullFlow(t *testing.T) {
	t.Setenv(passwordEnvVar, "correct-horse-battery-staple")
	srv := newAuthTestServer(t, false)
	handler := srv.routes()

	// Unauthenticated request to a protected page redirects to /login.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/plan", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("GET /plan unauthenticated = %d %q, want a redirect to /login", rec.Code, rec.Header().Get("Location"))
	}

	// The login page renders the password form, not the OIDC button.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/login", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="password"`) {
		t.Fatalf("GET /login did not render a password form: %d %q", rec.Code, rec.Body.String())
	}

	// Wrong password: no session cookie, form re-rendered with an error.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=nope&return_to=/plan"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Incorrect password") {
		t.Fatalf("POST /login with wrong password = %d %q, want the error re-rendered", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("wrong password should not set a session cookie")
	}

	// Correct password: redirects to return_to and sets a session cookie.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=correct-horse-battery-staple&return_to=/plan"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/plan" {
		t.Fatalf("POST /login with correct password = %d %q, want a redirect to /plan", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != authSessionCookie {
		t.Fatalf("expected a %s cookie, got %+v", authSessionCookie, cookies)
	}

	// The session cookie now grants access to the protected page.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/plan", nil)
	req.AddCookie(cookies[0])
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /plan with session cookie = %d, want 200", rec.Code)
	}
}

func TestPasswordAuth_BypassedWhenOIDCEnabled(t *testing.T) {
	srv := newAuthTestServer(t, true)
	handler := srv.routes()

	if srv.password != "" {
		t.Fatalf("password should never be resolved when OIDC is enabled, got %q", srv.password)
	}

	// The login page shows the OIDC button, not a password form.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `name="password"`) {
		t.Fatal("login page should not render a password form when OIDC is enabled")
	}

	// Posting to /login while OIDC is enabled must not grant a session.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=anything&return_to=/plan"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ServeHTTP(rec, req)
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("POST /login must not create a session while OIDC is enabled")
	}
}
