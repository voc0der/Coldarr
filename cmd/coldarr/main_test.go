package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// runColdarr runs the coldarr command line with args the way main does and
// returns what it printed to stdout.
func runColdarr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(args)
	var err error
	out := captureStdout(t, func() { err = root.Execute() })
	return out, err
}

// captureStdout returns everything fn prints to os.Stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	_ = w.Close()
	return <-out
}

// withStdin feeds input to anything reading os.Stdin until the test ends.
func withStdin(t *testing.T, input string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = orig
		_ = f.Close()
	})
}

// testConfig writes a coldarr.yaml with a hot and a cold tier on fresh
// directories, scored so that a long-held movie is planned for cold
// storage, and returns its path and the two tier directories.
func testConfig(t *testing.T) (cfgPath, hotDir, coldDir string) {
	t.Helper()
	dir := t.TempDir()
	hotDir, coldDir = filepath.Join(dir, "hot"), filepath.Join(dir, "cold")
	for _, d := range []string{hotDir, coldDir} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath = filepath.Join(dir, "coldarr.yaml")
	cfg := fmt.Sprintf(`tiers:
  - name: hot1
    role: hot
    paths: [%q]
    media_types: [movie, tv]
  - name: cold1
    role: cold
    paths: [%q]
    media_types: [movie, tv]
    max_used_percent: 99
    target_used_percent: 95
policy:
  cold_score_threshold: 20
  min_move_size_gb: 0.001
history:
  path: %q
`, hotDir, coldDir, filepath.Join(dir, "history.json"))
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// Moves confirm on the first check rather than waiting minutes for
	// real disk growth.
	t.Setenv("COLDARR_SETTLE_CHECK_INTERVAL", "10ms")
	t.Setenv("COLDARR_SETTLE_STABLE_CHECKS", "1")
	t.Setenv("COLDARR_SETTLE_MAX_WAIT", "3s")
	return cfgPath, hotDir, coldDir
}

// fakeRadarr serves the slice of Radarr's API the CLI uses, for a library
// of one movie - "Movie A" - that starts under root and moves wherever an
// editor request sends it.
type fakeRadarr struct {
	*httptest.Server

	mu           sync.Mutex
	root         string
	moves        []string // the rootFolderPath of each move requested
	moving       int      // started move commands to report
	commandsDown bool     // the command queue can't be read
	failMoves    bool
}

func newFakeRadarr(t *testing.T, root string) *fakeRadarr {
	t.Helper()
	f := &fakeRadarr{root: root}
	mux := http.NewServeMux()
	movie := func() map[string]any {
		f.mu.Lock()
		defer f.mu.Unlock()
		return map[string]any{
			"id": 1, "title": "Movie A", "titleSlug": "movie-a-2020",
			"path": f.root + "/Movie A", "rootFolderPath": f.root,
			"monitored": true, "hasFile": true, "status": "released",
			"added": "2020-01-01T00:00:00Z", "sizeOnDisk": 5_000_000,
		}
	}
	mux.HandleFunc("GET /api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{movie()})
	})
	mux.HandleFunc("GET /api/v3/movie/1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(movie())
	})
	for _, path := range []string{"/api/v3/tag", "/api/v3/qualityprofile"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[]`)) })
	}
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"records": []}`))
	})
	mux.HandleFunc("GET /api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version": "5.3.0"}`))
	})
	mux.HandleFunc("GET /api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n, down := f.moving, f.commandsDown
		f.mu.Unlock()
		if down {
			http.Error(w, "database is locked", http.StatusInternalServerError)
			return
		}
		cmds := []map[string]any{}
		for i := 0; i < n; i++ {
			cmds = append(cmds, map[string]any{"id": i + 1, "name": "MoveMovie", "status": "started"})
		}
		_ = json.NewEncoder(w).Encode(cmds)
	})
	mux.HandleFunc("POST /api/v3/command", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": 1, "status": "completed"}`))
	})
	mux.HandleFunc("GET /api/v3/command/1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id": 1, "status": "completed"}`))
	})
	mux.HandleFunc("PUT /api/v3/movie/editor", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RootFolderPath string `json:"rootFolderPath"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failMoves {
			http.Error(w, "destination not writable", http.StatusInternalServerError)
			return
		}
		f.moves = append(f.moves, req.RootFolderPath)
		f.root = req.RootFolderPath
		_, _ = w.Write([]byte(`[]`))
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRadarr) movesRequested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.moves...)
}

// connectRadarr stores a Radarr connection beside cfgPath, the way
// `coldarr connections set` does.
func connectRadarr(t *testing.T, cfgPath, url string) {
	t.Helper()
	if _, err := runColdarr(t, "--config", cfgPath, "connections", "set", "radarr", "--url", url, "--api-key", "test"); err != nil {
		t.Fatalf("connections set radarr: %v", err)
	}
}

func TestVersion(t *testing.T) {
	out, err := runColdarr(t, "version")
	if err != nil || strings.TrimSpace(out) != version {
		t.Fatalf("coldarr version = (%q, %v), want %q", out, err, version)
	}
}

// TestConfigPath_DefaultsToCOLDARR_CONFIG: with no --config flag, the
// config (and the connection store beside it) comes from COLDARR_CONFIG,
// which is how the Docker image points Coldarr at /config.
func TestConfigPath_DefaultsToCOLDARR_CONFIG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COLDARR_CONFIG", filepath.Join(dir, "coldarr.yaml"))

	if _, err := runColdarr(t, "connections", "list"); err != nil {
		t.Fatalf("connections list: %v", err)
	}
	if configPath != filepath.Join(dir, "coldarr.yaml") {
		t.Errorf("configPath = %q, want COLDARR_CONFIG's value", configPath)
	}
	if _, err := os.Stat(filepath.Join(dir, ".coldarr.key")); err != nil {
		t.Errorf("expected the connection store created beside COLDARR_CONFIG: %v", err)
	}
}

func TestLoadEngine_NeedsALibraryAndAValidConfig(t *testing.T) {
	cfgPath, _, _ := testConfig(t)
	if _, err := runColdarr(t, "--config", cfgPath, "report"); err == nil || !strings.Contains(err.Error(), "no Radarr or Sonarr connection is configured") {
		t.Errorf("report with no library configured = %v, want it to say how to configure one", err)
	}

	broken := filepath.Join(t.TempDir(), "coldarr.yaml")
	if err := os.WriteFile(broken, []byte("tiers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runColdarr(t, "--config", broken, "plan"); err == nil || !strings.Contains(err.Error(), "at least one tier must be configured") {
		t.Errorf("plan with no tiers = %v, want the config rejected", err)
	}
}
