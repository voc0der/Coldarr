package secrets

import (
	"os"
	"path/filepath"
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

func TestDelete_RemovesStoredConnection(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := s.Set("radarr", Connection{URL: "http://radarr:7878", APIKey: "a"}); err != nil {
		t.Fatalf("Set radarr: %v", err)
	}
	if err := s.Set("sonarr", Connection{URL: "http://sonarr:8989", APIKey: "b"}); err != nil {
		t.Fatalf("Set sonarr: %v", err)
	}
	if err := s.Delete("radarr"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	reloaded, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate (reload): %v", err)
	}
	if _, ok := reloaded.Get("radarr"); ok {
		t.Error("a deleted connection must stay deleted after a reload")
	}
	if _, source := reloaded.Effective("radarr"); source != SourceNone {
		t.Errorf("Effective(radarr) source = %q after delete, want %q", source, SourceNone)
	}
	if _, ok := reloaded.Get("sonarr"); !ok {
		t.Error("deleting one app's connection must leave the others alone")
	}
}

// TestEffective_JellyfinEnabledEnvVar: Jellyfin is the one app that can be
// switched off while configured, and from env alone that takes
// JELLYFIN_ENABLED - an unparseable value leaves it on rather than guessing.
func TestEffective_JellyfinEnabledEnvVar(t *testing.T) {
	s, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	t.Setenv("JELLYFIN_URL", "http://jellyfin:8096")
	t.Setenv("JELLYFIN_API_KEY", "env-key")

	for value, want := range map[string]bool{"": true, "false": false, "0": false, "true": true, "maybe": true} {
		t.Setenv("JELLYFIN_ENABLED", value)
		conn, source := s.Effective("jellyfin")
		if source != SourceEnv || conn.Enabled != want {
			t.Errorf("JELLYFIN_ENABLED=%q: got (enabled %v, source %q), want (enabled %v, source %q)", value, conn.Enabled, source, want, SourceEnv)
		}
	}
}

// TestEffective_StoredJellyfinKeepsItsEnabledFlag: unlike Radarr/Sonarr,
// which count as enabled whenever configured, a stored Jellyfin connection
// switched off in the GUI stays off.
func TestEffective_StoredJellyfinKeepsItsEnabledFlag(t *testing.T) {
	s, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := s.Set("jellyfin", Connection{URL: "http://jellyfin:8096", APIKey: "k", Enabled: false}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("radarr", Connection{URL: "http://radarr:7878", APIKey: "k", Enabled: false}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	if conn, source := s.Effective("jellyfin"); source != SourceStored || conn.Enabled {
		t.Errorf("Effective(jellyfin) = (enabled %v, %q), want a stored, disabled connection", conn.Enabled, source)
	}
	if conn, _ := s.Effective("radarr"); !conn.Enabled {
		t.Error("a configured Radarr connection is always enabled")
	}
}

// TestLoadOrCreate_ReplacedKeyIsAnError: the encryption key lives beside
// the ciphertext, and a key that was replaced or lost makes every stored
// connection unreadable. That has to stop startup with an error naming the
// key file, not come up looking like nothing was ever configured.
func TestLoadOrCreate_ReplacedKeyIsAnError(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := s.Set("radarr", Connection{URL: "http://radarr:7878", APIKey: "secret"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	keyPath := filepath.Join(dir, ".coldarr.key")
	if err := os.WriteFile(keyPath, make([]byte, 32), 0o600); err != nil {
		t.Fatalf("replacing key: %v", err)
	}

	_, err = LoadOrCreate(dir)
	if err == nil {
		t.Fatal("expected loading with a different key to fail")
	}
	if !strings.Contains(err.Error(), `decrypting stored connection for "radarr"`) || !strings.Contains(err.Error(), keyPath) {
		t.Fatalf("LoadOrCreate error = %v, want it to name the app and the key file", err)
	}
}

func TestLoadOrCreate_RejectsDamagedFiles(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(dir string) error
		wantErr string
	}{
		{
			name: "truncated key",
			setup: func(dir string) error {
				return os.WriteFile(filepath.Join(dir, ".coldarr.key"), []byte("short"), 0o600)
			},
			wantErr: "is not 32 bytes",
		},
		{
			name:    "unreadable key",
			setup:   func(dir string) error { return os.Mkdir(filepath.Join(dir, ".coldarr.key"), 0o750) },
			wantErr: "reading encryption key",
		},
		{
			name: "corrupt connection store",
			setup: func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "connections.enc.json"), []byte("{"), 0o600)
			},
			wantErr: "parsing",
		},
		{
			name:    "unreadable connection store",
			setup:   func(dir string) error { return os.Mkdir(filepath.Join(dir, "connections.enc.json"), 0o750) },
			wantErr: "reading",
		},
		{
			name: "connection store with a tampered entry",
			setup: func(dir string) error {
				if _, err := LoadOrCreate(dir); err != nil { // generates the key
					return err
				}
				// A well-formed 12-byte nonce, but ciphertext that was never
				// sealed under this key - what bit rot or a hand edit leaves.
				return os.WriteFile(filepath.Join(dir, "connections.enc.json"), []byte(`{"radarr": {"nonce": "AAAAAAAAAAAAAAAA", "ciphertext": "AAAAAAAAAAAAAAAAAAAAAA=="}}`), 0o600)
			},
			wantErr: `decrypting stored connection for "radarr"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := tt.setup(dir); err != nil {
				t.Fatalf("setup: %v", err)
			}
			if _, err := LoadOrCreate(dir); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadOrCreate error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadOrCreate_EmptyStoreAndMissingDirectory(t *testing.T) {
	// A config directory that doesn't exist yet is created along with the key.
	dir := filepath.Join(t.TempDir(), "config")
	s, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if err := s.Set("radarr", Connection{URL: "http://radarr:7878", APIKey: "k"}); err != nil {
		t.Fatalf("Set into a freshly created directory: %v", err)
	}

	// A zero-byte store is a store with nothing saved yet.
	if err := os.WriteFile(filepath.Join(dir, "connections.enc.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate (empty store): %v", err)
	}
	if _, ok := reloaded.Get("radarr"); ok {
		t.Error("an empty connection store should load with nothing in it")
	}

	// A key directory that can't be created is reported, not ignored.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(filepath.Join(blocker, "config")); err == nil {
		t.Error("expected an error when the config directory can't be created")
	}
}

func TestSet_ReportsAFailedSave(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	if err := os.Mkdir(filepath.Join(dir, "connections.enc.json.tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("radarr", Connection{URL: "http://radarr:7878", APIKey: "k"}); err == nil || !strings.Contains(err.Error(), "writing") {
		t.Fatalf("Set error = %v, want a failed temp-file write", err)
	}
	if err := os.Remove(filepath.Join(dir, "connections.enc.json.tmp")); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(filepath.Join(dir, "connections.enc.json"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("radarr"); err == nil || !strings.Contains(err.Error(), "saving") {
		t.Fatalf("Delete error = %v, want a failed rename into place", err)
	}
}
