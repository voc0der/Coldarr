package mover

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/planner"
)

func TestSettleTracker_DoesNotSettleBeforeGrowth(t *testing.T) {
	tr := newSettleTracker(100, 100) // targetGrowth = 90
	if tr.observe(100, 2) {
		t.Fatal("should not settle before any growth is observed")
	}
	if tr.observe(100, 2) {
		t.Fatal("should not settle before any growth, no matter how many unchanged readings")
	}
}

func TestSettleTracker_RequiresStabilityAfterGrowth(t *testing.T) {
	tr := newSettleTracker(100, 100) // targetGrowth = 90, so grown once used >= 190
	if tr.observe(195, 2) {
		t.Fatal("should not settle on the very reading it first grows - still needs to be stable")
	}
	if tr.observe(195, 2) {
		t.Fatal("only one stable reading since growth, needs two")
	}
	if !tr.observe(195, 2) {
		t.Fatal("expected settled after growth plus two stable readings")
	}
}

func TestSettleTracker_ResetsStabilityOnChange(t *testing.T) {
	tr := newSettleTracker(0, 100) // targetGrowth = 90
	tr.observe(90, 3)              // grows
	tr.observe(90, 3)              // stable x1
	tr.observe(95, 3)              // still growing - resets stability count
	if tr.observe(95, 3) {
		t.Fatal("only one stable reading since the last change, should not settle yet")
	}
	if tr.observe(95, 3) {
		t.Fatal("only two stable readings, needs three")
	}
	if !tr.observe(95, 3) {
		t.Fatal("expected settled after three stable readings")
	}
}

func TestSettleTracker_ZeroSizeItemSettlesOnFirstStableReading(t *testing.T) {
	tr := newSettleTracker(50, 0) // targetGrowth = 0
	if !tr.observe(50, 1) {
		t.Fatal("expected immediate settle for a zero-size item with stableTarget 1")
	}
}

func TestVolumeKey(t *testing.T) {
	volumeOf := map[string]uint64{"/a": 1, "/b": 1, "/c": 2}

	if volumeKey("/a", volumeOf) != volumeKey("/b", volumeOf) {
		t.Fatal("expected /a and /b (same device) to produce the same key")
	}
	if volumeKey("/a", volumeOf) == volumeKey("/c", volumeOf) {
		t.Fatal("expected /a and /c (different devices) to produce different keys")
	}
	if volumeKey("/unknown1", volumeOf) == volumeKey("/unknown2", volumeOf) {
		t.Fatal("paths with no known device must never be merged with each other")
	}
}

// TestApply_SerializesSameVolumeButParallelAcrossVolumes is the core
// safety property this package exists for: two items destined for
// different tier paths that are really the same physical volume must
// never be moved concurrently, while items on a genuinely different
// volume proceed without waiting for it.
func TestApply_SerializesSameVolumeButParallelAcrossVolumes(t *testing.T) {
	var mu sync.Mutex
	var callOrder []string
	blockedOnce := map[string]bool{}
	volAGate := make(chan struct{})

	// Only /volA/movies' first settle-check ever blocks (simulating a
	// slow first transfer); everything else - including /volA/movies on
	// any later call, and /volA/tv, and /volB entirely - settles on its
	// first reading.
	statFunc := func(path string) (diskusage.Usage, error) {
		mu.Lock()
		shouldBlock := path == "/volA/movies" && !blockedOnce[path]
		blockedOnce[path] = true
		mu.Unlock()

		if shouldBlock {
			<-volAGate
		}
		return diskusage.Usage{TotalBytes: 100, UsedBytes: 50, FreeBytes: 50, UsedPercent: 50}, nil
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			MovieIDs       []int  `json:"movieIds"`
			RootFolderPath string `json:"rootFolderPath"`
		}
		_ = json.Unmarshal(body, &req)

		mu.Lock()
		callOrder = append(callOrder, fmt.Sprintf("%d@%s", req.MovieIDs[0], req.RootFolderPath))
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hist, err := history.Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}

	m := &Movers{
		Radarr:              arrapi.NewRadarrClient(srv.URL, "key"),
		History:             hist,
		SettleCheckInterval: time.Millisecond,
		SettleStableChecks:  1,
		statFunc:            statFunc,
	}

	plan := &planner.Plan{
		Entries: []planner.MoveEntry{
			{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Item1-volA-movies"}, ToTier: "cold-a", ToPath: "/volA/movies"},
			{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Item2-volA-tv"}, ToTier: "cold-a", ToPath: "/volA/tv"},
			{Item: model.MediaItem{ArrApp: "radarr", ID: 3, Title: "Item3-volB"}, ToTier: "cold-b", ToPath: "/volB"},
		},
	}
	volumeOf := map[string]uint64{
		"/volA/movies": 1,
		"/volA/tv":     1, // same device as /volA/movies, different tier path
		"/volB":        2,
	}

	progress := m.Apply(plan, volumeOf)

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(callOrder) >= 2
	})

	mu.Lock()
	gotCalls := append([]string(nil), callOrder...)
	mu.Unlock()

	if !containsAll(gotCalls, "1@/volA/movies", "3@/volB") {
		t.Fatalf("expected item1 (volA) and item3 (volB) to both have started without waiting on each other, got %v", gotCalls)
	}
	if contains(gotCalls, "2@/volA/tv") {
		t.Fatalf("item2 must not start before item1 (same volume) has settled, got %v", gotCalls)
	}

	close(volAGate)
	progress.Wait()

	snap := progress.Snapshot()
	if failed := snap.Failed(); len(failed) != 0 {
		t.Fatalf("expected no failures, got %+v", failed)
	}
	if moved := snap.Moved(); len(moved) != 3 {
		t.Fatalf("expected all 3 items moved, got %d", len(moved))
	}

	mu.Lock()
	finalCalls := append([]string(nil), callOrder...)
	mu.Unlock()

	i1, i2 := indexOf(finalCalls, "1@/volA/movies"), indexOf(finalCalls, "2@/volA/tv")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("expected item1 called before item2 (same volume, serialized), got order %v", finalCalls)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func containsAll(haystack []string, needles ...string) bool {
	for _, n := range needles {
		if !contains(haystack, n) {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	return indexOf(haystack, needle) >= 0
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}
