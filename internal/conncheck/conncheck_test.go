package conncheck

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vocoder/coldarr/internal/secrets"
)

func TestValid(t *testing.T) {
	for _, app := range []string{"radarr", "sonarr", "lidarr", "jellyfin", "qbittorrent"} {
		if !Valid(app) {
			t.Errorf("expected %q to be valid", app)
		}
	}
	if Valid("plex") {
		t.Error("expected plex to be invalid - not yet supported")
	}
}

func TestTest_UnknownApp(t *testing.T) {
	if _, err := Test("plex", secrets.Connection{}); err == nil {
		t.Fatal("expected an error for an unknown app")
	}
}

func TestTest_Radarr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version": "5.1.0"}`))
	}))
	defer srv.Close()

	version, err := Test("radarr", secrets.Connection{URL: srv.URL, APIKey: "key"})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if version != "5.1.0" {
		t.Fatalf("version = %q, want 5.1.0", version)
	}
}

func TestTest_Sonarr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version": "4.0.1"}`))
	}))
	defer srv.Close()

	version, err := Test("sonarr", secrets.Connection{URL: srv.URL, APIKey: "key"})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if version != "4.0.1" {
		t.Fatalf("version = %q, want 4.0.1", version)
	}
}

func TestTest_Jellyfin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Version": "10.9.0", "ServerName": "home"}`))
	}))
	defer srv.Close()

	version, err := Test("jellyfin", secrets.Connection{URL: srv.URL, APIKey: "key"})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if version != "10.9.0" {
		t.Fatalf("version = %q, want 10.9.0", version)
	}
}

func TestTest_UnreachableFails(t *testing.T) {
	if _, err := Test("radarr", secrets.Connection{URL: "http://127.0.0.1:1", APIKey: "key"}); err == nil {
		t.Fatal("expected an error connecting to an unreachable address")
	}
}

func TestTest_Lidarr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/system/status" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version": "2.9.6"}`))
	}))
	defer srv.Close()

	if version, err := Test("lidarr", secrets.Connection{URL: srv.URL, APIKey: "key"}); err != nil || version != "2.9.6" {
		t.Fatalf("Test = %q, %v; want Lidarr's v1 status answered", version, err)
	}
}

// TestTest_Qbittorrent: the test signs in with the stored login, the way
// every later request will.
func TestTest_Qbittorrent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			_ = r.ParseForm()
			if r.Form.Get("username") != "admin" || r.Form.Get("password") != "secret" {
				_, _ = w.Write([]byte("Fails."))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "s", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}) //nolint:gosec // qBittorrent over plain HTTP sets no Secure flag, and this fakes it
			_, _ = w.Write([]byte("Ok."))
		case "/api/v2/app/version":
			if c, err := r.Cookie("SID"); err != nil || c.Value != "s" {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte("v4.6.7"))
		}
	}))
	defer srv.Close()

	if version, err := Test("qbittorrent", secrets.Connection{URL: srv.URL, Username: "admin", Password: "secret"}); err != nil || version != "v4.6.7" {
		t.Fatalf("Test = %q, %v; want the version after signing in", version, err)
	}
	if _, err := Test("qbittorrent", secrets.Connection{URL: srv.URL, Username: "admin", Password: "nope"}); err == nil {
		t.Fatal("a rejected login must fail the test")
	}
}
