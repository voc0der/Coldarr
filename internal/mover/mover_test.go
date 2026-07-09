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

// TestApply_FillingWaitsForFreeingAcrossDifferentVolumes proves the other
// safety property added for near-full cold tiers: a lower-Phase move
// (e.g. reclaiming a grow-risk item back to hot, freeing cold capacity a
// later move depends on) must fully land and settle before any
// higher-Phase move starts writing - even though they land on entirely
// different volumes and, absent this ordering, would be free to run
// fully concurrently like TestApply_SerializesSameVolumeButParallelAcrossVolumes
// above proves for same-phase moves.
func TestApply_FillingWaitsForFreeingAcrossDifferentVolumes(t *testing.T) {
	var mu sync.Mutex
	var callOrder []string
	freeingBlocked := false
	freeGate := make(chan struct{})

	// Only /hot's first settle-check ever blocks (simulating a slow
	// reclaim settling); /cold always settles on its first reading.
	statFunc := func(path string) (diskusage.Usage, error) {
		mu.Lock()
		shouldBlock := path == "/hot" && !freeingBlocked
		freeingBlocked = true
		mu.Unlock()

		if shouldBlock {
			<-freeGate
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
			{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Reclaimed"}, ToTier: "hot", ToPath: "/hot", Phase: 0},
			{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Backfilled"}, ToTier: "cold", ToPath: "/cold", Phase: 1},
		},
	}

	progress := m.Apply(plan, nil)

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(callOrder) >= 1
	})

	mu.Lock()
	gotCalls := append([]string(nil), callOrder...)
	mu.Unlock()

	if !contains(gotCalls, "1@/hot") {
		t.Fatalf("expected the freeing move to have started, got %v", gotCalls)
	}
	if contains(gotCalls, "2@/cold") {
		t.Fatalf("filling move must not start before the freeing move has settled, got %v", gotCalls)
	}

	close(freeGate)
	progress.Wait()

	snap := progress.Snapshot()
	if failed := snap.Failed(); len(failed) != 0 {
		t.Fatalf("expected no failures, got %+v", failed)
	}
	if moved := snap.Moved(); len(moved) != 2 {
		t.Fatalf("expected both items moved, got %d", len(moved))
	}

	mu.Lock()
	finalCalls := append([]string(nil), callOrder...)
	mu.Unlock()

	i1, i2 := indexOf(finalCalls, "1@/hot"), indexOf(finalCalls, "2@/cold")
	if i1 < 0 || i2 < 0 || i1 > i2 {
		t.Fatalf("expected the freeing move (1) called before the filling move (2), got order %v", finalCalls)
	}
}

// TestApply_ThreePhasesExecuteInStrictOrder proves genuine N-phase
// sequencing (not just the 2-phase free/fill case above): three entries
// on three different destination volumes, phases 0/1/2, each gated so it
// only settles once explicitly released. Phase 1 must never start before
// phase 0 is released, and phase 2 must never start before phase 1 is
// released - this is what lets a fixpoint-planned entry (see
// planner.Build) that only became possible because an earlier round's
// move already landed for real, not just on paper.
func TestApply_ThreePhasesExecuteInStrictOrder(t *testing.T) {
	var mu sync.Mutex
	var callOrder []string
	blocked := map[string]bool{}
	gate0 := make(chan struct{})
	gate1 := make(chan struct{})
	gate2 := make(chan struct{})
	gateFor := map[string]chan struct{}{"/p0": gate0, "/p1": gate1, "/p2": gate2}

	statFunc := func(path string) (diskusage.Usage, error) {
		mu.Lock()
		gate, hasGate := gateFor[path]
		shouldBlock := hasGate && !blocked[path]
		blocked[path] = true
		mu.Unlock()

		if shouldBlock {
			<-gate
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
			{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Item0"}, ToTier: "t0", ToPath: "/p0", Phase: 0},
			{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Item1"}, ToTier: "t1", ToPath: "/p1", Phase: 1},
			{Item: model.MediaItem{ArrApp: "radarr", ID: 3, Title: "Item2"}, ToTier: "t2", ToPath: "/p2", Phase: 2},
		},
	}

	progress := m.Apply(plan, nil)

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(callOrder) >= 1
	})
	mu.Lock()
	got := append([]string(nil), callOrder...)
	mu.Unlock()
	if !contains(got, "1@/p0") {
		t.Fatalf("expected phase 0's move to have started, got %v", got)
	}
	if contains(got, "2@/p1") || contains(got, "3@/p2") {
		t.Fatalf("phase 1/2 must not start before phase 0 settles, got %v", got)
	}

	close(gate0)
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(callOrder) >= 2
	})
	mu.Lock()
	got = append([]string(nil), callOrder...)
	mu.Unlock()
	if !contains(got, "2@/p1") {
		t.Fatalf("expected phase 1's move to have started once phase 0 settled, got %v", got)
	}
	if contains(got, "3@/p2") {
		t.Fatalf("phase 2 must not start before phase 1 settles, got %v", got)
	}

	close(gate1)
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(callOrder) >= 3
	})

	close(gate2)
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
	i0, i1, i2 := indexOf(finalCalls, "1@/p0"), indexOf(finalCalls, "2@/p1"), indexOf(finalCalls, "3@/p2")
	if i0 < 0 || i1 < 0 || i2 < 0 || !(i0 < i1 && i1 < i2) {
		t.Fatalf("expected strict phase order 0 < 1 < 2, got %v", finalCalls)
	}
}

// TestSettle_ConfirmsViaRescanWhenUsageNeverGrows covers the real-world bug
// this fallback exists for: a move that lands without adding any
// measurable usage to the destination (the item was already at/near
// there, or landed via a same-volume rename) would otherwise never look
// "grown" and wait out the full SettleMaxWait - here set to an hour -
// every single time, even though the file is already correctly sitting at
// its destination. confirmLanded's Radarr rescan+GetMovieSize fallback
// must recognize it as done almost immediately instead.
func TestSettle_ConfirmsViaRescanWhenUsageNeverGrows(t *testing.T) {
	const toPath = "/cold/movies"

	statFunc := func(path string) (diskusage.Usage, error) {
		return diskusage.Usage{TotalBytes: 100, UsedBytes: 50, FreeBytes: 50, UsedPercent: 50}, nil
	}

	var mu sync.Mutex
	rescanCalls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v3/movie/editor":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			mu.Lock()
			rescanCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": "completed"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/command/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": "completed"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "sizeOnDisk": 10, "path": toPath + "/Item1"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
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
		SettleStableChecks:  2,
		SettleMaxWait:       time.Hour,
		statFunc:            statFunc,
	}

	plan := &planner.Plan{
		Entries: []planner.MoveEntry{
			{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Item1", SizeBytes: 10}, ToTier: "cold", ToPath: toPath},
		},
	}

	progress := m.Apply(plan, nil)
	waitFor(t, 2*time.Second, func() bool { return progress.Snapshot().Done })

	snap := progress.Snapshot()
	if failed := snap.Failed(); len(failed) != 0 {
		t.Fatalf("expected no failures, got %+v", failed)
	}
	if moved := snap.Moved(); len(moved) != 1 {
		t.Fatalf("expected item moved, got %d", len(moved))
	}

	mu.Lock()
	got := rescanCalls
	mu.Unlock()
	if got == 0 {
		t.Fatal("expected at least one rescan call as the fallback confirmation path - settle() must not rely on disk-usage growth alone")
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
