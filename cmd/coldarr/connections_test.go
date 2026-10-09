package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/secrets"
)

// TestConnectionsCommands walks a connection through its whole life from
// the command line: stored, listed (key masked), tested, and deleted.
func TestConnectionsCommands(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "coldarr.yaml")
	radarr := newFakeRadarr(t, "/hot")
	run := func(args ...string) (string, error) {
		t.Helper()
		return runColdarr(t, append([]string{"--config", cfgPath, "connections"}, args...)...)
	}

	out, err := run("list")
	if err != nil {
		t.Fatalf("connections list: %v", err)
	}
	for _, app := range []string{"radarr", "sonarr", "jellyfin"} {
		if !strings.Contains(out, app) || strings.Count(out, "not configured") != 3 {
			t.Fatalf("list on a fresh install = %q, want all three not configured", out)
		}
	}

	if out, err := run("set", "radarr", "--url", radarr.URL, "--api-key", "abcd1234"); err != nil || !strings.Contains(out, "saved radarr connection") {
		t.Fatalf("connections set = (%q, %v), want it saved", out, err)
	}
	if out, err := run("set", "jellyfin", "--url", "http://jellyfin:8096", "--api-key", "jf", "--disabled"); err != nil || !strings.Contains(out, "saved jellyfin connection") {
		t.Fatalf("connections set jellyfin = (%q, %v), want it saved", out, err)
	}

	out, err = run("list")
	if err != nil {
		t.Fatalf("connections list: %v", err)
	}
	if !strings.Contains(out, radarr.URL+" (source: stored, enabled: true, key: ****1234)") {
		t.Errorf("list = %q, want radarr shown with its key masked", out)
	}
	if !strings.Contains(out, "enabled: false, key: **)") {
		t.Errorf("list = %q, want jellyfin shown stored but disabled", out)
	}
	if strings.Contains(out, "abcd1234") {
		t.Error("list must never print a whole API key")
	}

	if out, err := run("test", "radarr"); err != nil || !strings.Contains(out, "radarr: connected (version 5.3.0, source: stored)") {
		t.Fatalf("connections test = (%q, %v), want a successful test", out, err)
	}

	if out, err := run("delete", "radarr"); err != nil || !strings.Contains(out, "deleted stored radarr connection") {
		t.Fatalf("connections delete = (%q, %v), want it deleted", out, err)
	}
	if _, err := run("test", "radarr"); err == nil || !strings.Contains(err.Error(), "radarr is not configured") {
		t.Fatalf("testing a deleted connection = %v, want it reported as not configured", err)
	}
}

func TestConnectionsCommands_RejectBadInput(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "coldarr.yaml")
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(failing.Close)

	tests := []struct {
		args    []string
		wantErr string
	}{
		{args: []string{"set", "lidarr", "--url", "http://lidarr:8686", "--api-key", "k"}, wantErr: `unknown app "lidarr"`},
		{args: []string{"set", "radarr", "--api-key", "k"}, wantErr: "--url and --api-key are required"},
		{args: []string{"set", "radarr", "--url", "http://radarr:7878"}, wantErr: "--url and --api-key are required"},
		{args: []string{"test", "lidarr"}, wantErr: `unknown app "lidarr"`},
		{args: []string{"delete", "lidarr"}, wantErr: `unknown app "lidarr"`},
		{args: []string{"set"}, wantErr: "accepts 1 arg"},
	}
	for _, tt := range tests {
		if _, err := runColdarr(t, append([]string{"--config", cfgPath, "connections"}, tt.args...)...); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("connections %v = %v, want an error containing %q", tt.args, err, tt.wantErr)
		}
	}

	// A connection that doesn't answer properly fails its test, naming the app.
	if _, err := runColdarr(t, "--config", cfgPath, "connections", "set", "sonarr", "--url", failing.URL, "--api-key", "wrong"); err != nil {
		t.Fatal(err)
	}
	if _, err := runColdarr(t, "--config", cfgPath, "connections", "test", "sonarr"); err == nil || !strings.HasPrefix(err.Error(), "sonarr: ") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("testing a rejected connection = %v, want sonarr's 401", err)
	}
}

func TestMaskKey(t *testing.T) {
	for key, want := range map[string]string{"": "(none)", "abc": "***", "abcd": "****", "abcdef123456": "********3456"} {
		if got := maskKey(key); got != want {
			t.Errorf("maskKey(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestConnectionsSet_KeepsTheExternalURL: only the web GUI sets a
// connection's External URL, so `connections set` must leave it alone
// rather than replace the whole stored connection.
func TestConnectionsSet_KeepsTheExternalURL(t *testing.T) {
	dir := t.TempDir()
	store, err := secrets.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("radarr", secrets.Connection{URL: "http://old:7878", APIKey: "old", Enabled: true, ExternalURL: "https://radarr.example.com"}); err != nil {
		t.Fatal(err)
	}

	if _, err := runColdarr(t, "--config", filepath.Join(dir, "coldarr.yaml"), "connections", "set", "radarr", "--url", "http://radarr:7878", "--api-key", "new"); err != nil {
		t.Fatalf("connections set: %v", err)
	}

	reloaded, err := secrets.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reloaded.Get("radarr"); got != (secrets.Connection{URL: "http://radarr:7878", APIKey: "new", Enabled: true, ExternalURL: "https://radarr.example.com"}) {
		t.Fatalf("stored radarr = %+v, want the new URL and key with the External URL kept", got)
	}
}
