package secrets

import (
	"os"
	"strings"
	"testing"
)

func TestSetGetRoundTrip(t *testing.T) {
	dir := t.TempDir()

	s, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	want := Connection{URL: "http://radarr:7878", APIKey: "supersecret"}
	if err := s.Set("radarr", want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Reload from disk to prove it persisted and decrypts correctly, not
	// just that the in-memory map still has it.
	reloaded, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate (reload): %v", err)
	}

	got, ok := reloaded.Get("radarr")
	if !ok {
		t.Fatalf("expected stored connection to be present after reload")
	}
	if got.URL != want.URL || got.APIKey != want.APIKey {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestStoredOnDiskIsNotPlaintext(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	secretValue := "do-not-leak-this-api-key"
	if err := s.Set("radarr", Connection{URL: "http://radarr:7878", APIKey: secretValue}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, err := os.ReadFile(dir + "/connections.enc.json")
	if err != nil {
		t.Fatalf("reading connections.enc.json: %v", err)
	}
	if strings.Contains(string(raw), secretValue) {
		t.Fatalf("API key appears in plaintext on disk: %s", raw)
	}
}

func TestEffective_EnvOverridesStored(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := s.Set("radarr", Connection{URL: "http://stored:7878", APIKey: "stored-key"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	t.Setenv("RADARR_URL", "http://env:7878")
	t.Setenv("RADARR_API_KEY", "env-key")

	conn, source := s.Effective("radarr")
	if source != SourceEnv {
		t.Fatalf("expected source %q, got %q", SourceEnv, source)
	}
	if conn.URL != "http://env:7878" || conn.APIKey != "env-key" {
		t.Fatalf("expected env values to win, got %+v", conn)
	}
}

func TestEffective_FallsBackToStoredWithoutEnv(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := s.Set("sonarr", Connection{URL: "http://sonarr:8989", APIKey: "abc"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	conn, source := s.Effective("sonarr")
	if source != SourceStored {
		t.Fatalf("expected source %q, got %q", SourceStored, source)
	}
	if conn.URL != "http://sonarr:8989" {
		t.Fatalf("unexpected conn: %+v", conn)
	}
}

func TestEffective_NoneWhenNothingConfigured(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	_, source := s.Effective("jellyfin")
	if source != SourceNone {
		t.Fatalf("expected source %q, got %q", SourceNone, source)
	}
}
