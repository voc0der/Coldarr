package conncheck

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vocoder/coldarr/internal/secrets"
)

func TestValid(t *testing.T) {
	for _, app := range []string{"radarr", "sonarr", "jellyfin"} {
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
