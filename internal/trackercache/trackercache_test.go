package trackercache

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/qbit"
)

// fakeTrackers serves /torrents/trackers from a map, counting lookups.
type fakeTrackers struct {
	*httptest.Server
	mu      sync.Mutex
	hosts   map[string][]string
	lookups map[string]int
	fail    map[string]bool
}

func newFakeTrackers(t *testing.T, hosts map[string][]string) *fakeTrackers {
	t.Helper()
	f := &fakeTrackers{hosts: hosts, lookups: map[string]int{}, fail: map[string]bool{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash := r.URL.Query().Get("hash")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lookups[hash]++
		if f.fail[hash] {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		hosts, ok := f.hosts[hash]
		if !ok {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		out := []map[string]any{{"url": "** [DHT] **"}}
		for _, h := range hosts {
			out = append(out, map[string]any{"url": "https://" + h + "/announce/PASSKEY"})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.Close)
	return f
}

// lookupsOfA counts how many times download "a" was looked up.
func (f *fakeTrackers) lookupsOfA() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups["a"]
}

func TestRefresh_LooksUpOnlyWhatItDoesntKnow(t *testing.T) {
	f := newFakeTrackers(t, map[string][]string{"a": {"tracker.example.org"}, "b": {"one.example.net", "two.example.net"}, "c": {}})
	path := filepath.Join(t.TempDir(), "trackers.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := qbit.NewClient(f.URL, qbit.Auth{})
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	list := []qbit.Torrent{{Hash: "a"}, {Hash: "b"}, {Hash: "c", TrackerHost: "live.example.com"}, {Hash: "gone"}}

	looked, err := s.Refresh(c, list, now)
	if err != nil || looked != 3 {
		t.Fatalf("Refresh = %d, %v; want 3 looked up and the vanished one skipped", looked, err)
	}
	snap := s.Get()
	if hosts, ok := snap.Hosts("b"); !ok || strings.Join(hosts, ",") != "one.example.net,two.example.net" {
		t.Errorf("Hosts(b) = %v, %v", hosts, ok)
	}
	if hosts, ok := snap.Hosts("c"); !ok || len(hosts) != 0 {
		t.Errorf("Hosts(c) = %v, %v; want looked up, with no trackers", hosts, ok)
	}
	if _, ok := snap.Hosts("gone"); ok {
		t.Error("a download qBittorrent didn't have can't be cached")
	}
	if got := strings.Join(snap.SeenHosts(), ","); got != "live.example.com,one.example.net,tracker.example.org,two.example.net" {
		t.Errorf("SeenHosts = %s, want every host, the live tracker field included", got)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "PASSKEY") {
		t.Fatal("an announce URL's passkey reached the cache file")
	}

	// A second scan looks nothing up again; a download that's gone is
	// forgotten, but its trackers stay in Seen.
	if looked, err := s.Refresh(c, []qbit.Torrent{{Hash: "a"}}, now.Add(time.Hour)); err != nil || looked != 0 {
		t.Fatalf("second Refresh = %d, %v", looked, err)
	}
	if f.lookupsOfA() != 1 {
		t.Errorf("a looked up %d times, want once", f.lookupsOfA())
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	snap = reloaded.Get()
	if _, ok := snap.Hosts("b"); ok || len(snap.Seen) != 4 {
		t.Errorf("after b left: Downloads = %v, Seen = %v; want b forgotten and its hosts still seen", snap.Downloads, snap.Seen)
	}

	// Past refreshAfter, it's looked up again.
	if _, err := reloaded.Refresh(c, []qbit.Torrent{{Hash: "a"}}, now.Add(31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if f.lookupsOfA() != 2 {
		t.Errorf("a looked up %d times after a month, want twice", f.lookupsOfA())
	}
}

func TestRefresh_KeepsWhatItResolvedWhenSomeFail(t *testing.T) {
	f := newFakeTrackers(t, map[string][]string{"a": {"tracker.example.org"}, "b": {"x.example.net"}})
	f.fail["b"] = true
	s, err := Load(filepath.Join(t.TempDir(), "trackers.json"))
	if err != nil {
		t.Fatal(err)
	}
	looked, err := s.Refresh(qbit.NewClient(f.URL, qbit.Auth{}), []qbit.Torrent{{Hash: "a"}, {Hash: "b"}}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "download b") || looked != 1 {
		t.Fatalf("Refresh = %d, %v; want a resolved and b's failure reported", looked, err)
	}
	snap := s.Get()
	if _, ok := snap.Hosts("a"); !ok || !snap.RefreshedAt.IsZero() {
		t.Errorf("snapshot = %+v, want a kept but the scan not marked complete", snap)
	}
}

func TestLoadAndNil(t *testing.T) {
	var s *Store
	if snap := s.Get(); len(snap.Downloads) != 0 {
		t.Error("a nil store is empty")
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(empty); err != nil {
		t.Errorf("empty file: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "parsing tracker cache") {
		t.Errorf("bad file = %v", err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "reading tracker cache") {
		t.Errorf("directory = %v", err)
	}

	blocked := filepath.Join(dir, "blocked.json")
	if err := os.Mkdir(blocked+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	s, err := Load(blocked)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeTrackers(t, map[string][]string{"a": {"t.example.org"}})
	if _, err := s.Refresh(qbit.NewClient(f.URL, qbit.Auth{}), []qbit.Torrent{{Hash: "a"}}, time.Now()); err == nil {
		t.Error("a cache that can't be saved should say so")
	}
}
