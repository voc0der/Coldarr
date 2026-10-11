package importcache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStore_RemembersConfirmationsAndForgetsGoneDownloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imports.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Imported("radarr", "ABC") {
		t.Fatal("a fresh cache knows nothing")
	}

	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	keep := map[string]bool{"ABC": true, "DEF": true}
	if err := s.Record([]Confirmation{{App: "radarr", Hash: "ABC"}, {App: "sonarr", Hash: "DEF"}}, keep, now); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Imported("radarr", "ABC") || !reloaded.Imported("sonarr", "DEF") {
		t.Fatal("confirmations should survive a reload")
	}
	if reloaded.Imported("sonarr", "ABC") {
		t.Error("a confirmation is per app")
	}

	// DEF is gone from qBittorrent; recording nothing new still prunes it.
	if err := reloaded.Record(nil, map[string]bool{"ABC": true}, now); err != nil {
		t.Fatal(err)
	}
	again, _ := Load(path)
	if again.Imported("sonarr", "DEF") || !again.Imported("radarr", "ABC") {
		t.Error("a download qBittorrent no longer has should be forgotten, and only that")
	}

	// Nothing changed: no write needed, so even a vanished directory is fine.
	info, _ := os.Stat(path)
	if err := again.Record([]Confirmation{{App: "radarr", Hash: "ABC"}}, map[string]bool{"ABC": true}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(info.ModTime()) {
		t.Error("a record with nothing new rewrote the file")
	}
}

func TestStore_NilAndBadFiles(t *testing.T) {
	var s *Store
	if s.Imported("radarr", "A") || s.Record([]Confirmation{{App: "radarr", Hash: "A"}}, nil, time.Now()) != nil {
		t.Error("a nil store remembers nothing and never fails")
	}

	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(empty); err != nil || s.Imported("radarr", "A") {
		t.Errorf("an empty file = %v, want an empty cache", err)
	}
	null := filepath.Join(dir, "null.json")
	if err := os.WriteFile(null, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(null); err != nil || s.Record([]Confirmation{{App: "radarr", Hash: "A"}}, map[string]bool{"A": true}, time.Now()) != nil {
		t.Errorf("a null file should load as empty and stay usable: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "parsing import cache") {
		t.Errorf("a corrupt file = %v, want a parse error", err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "reading import cache") {
		t.Errorf("an unreadable path = %v, want a read error", err)
	}

	// A directory where the temp file goes makes every save fail.
	blocked := filepath.Join(dir, "blocked.json")
	if err := os.Mkdir(blocked+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	s, err := Load(blocked)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Record([]Confirmation{{App: "radarr", Hash: "A"}}, map[string]bool{"A": true}, time.Now()); err == nil {
		t.Error("a cache that can't be written should say so")
	}
}

func TestHashOf(t *testing.T) {
	if got := hashOf("radarr|ABC"); got != "ABC" {
		t.Errorf("hashOf = %q", got)
	}
	if got := hashOf("nobar"); got != "nobar" {
		t.Errorf("hashOf without a separator = %q", got)
	}
}
