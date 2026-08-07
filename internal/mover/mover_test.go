package mover

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/planner"
)

// fakeRadarrMoveServer returns a Radarr-shaped test server that records
// every move (PUT /api/v3/movie/editor) into callOrder and truthfully
// answers RescanMovie/GetMovieSize based on the most recent move each
// movie ID actually received. Tests that only care about move ordering
// (not exercising a size mismatch specifically) can rely on this to
// satisfy confirmLanded's ground-truth check (see mover.go) without
// re-implementing routing for every endpoint by hand.
func fakeRadarrMoveServer(t *testing.T, mu *sync.Mutex, callOrder *[]string) *httptest.Server {
	t.Helper()
	lastPath := map[int]string{}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v3/movie/editor":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				MovieIDs       []int  `json:"movieIds"`
				RootFolderPath string `json:"rootFolderPath"`
			}
			_ = json.Unmarshal(body, &req)

			mu.Lock()
			for _, id := range req.MovieIDs {
				lastPath[id] = req.RootFolderPath
				*callOrder = append(*callOrder, fmt.Sprintf("%d@%s", id, req.RootFolderPath))
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": "completed"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/command/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": "completed"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v3/movie/"):
			id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/api/v3/movie/"))
			mu.Lock()
			path := lastPath[id]
			mu.Unlock()
			// A large sentinel sizeOnDisk keeps this helper generic: it
			// satisfies sizeLanded for any reasonably-sized test fixture
			// regardless of whether that test cares about Item.SizeBytes,
			// so callers can drive verifyRoom's pre-flight check (which
			// does care) without also having to hand-wire a size that
			// tracks each item's real progress. Size-mismatch itself is
			// covered precisely by TestSettle_RejectsPathMatchButShortSize
			// below, which uses its own bespoke server instead of this one.
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "sizeOnDisk": int64(1) << 40, "path": path})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

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

// TestVerifyRoom_RejectsWhenNotEnoughRealFreeSpace and its sibling below
// exercise verifyRoom directly, in isolation from the rest of Apply's
// machinery: it's a pure function of "what does the destination actually
// report right now," so it doesn't need a fake Radarr server or a full
// plan to prove its core logic.
func TestVerifyRoom_RejectsWhenNotEnoughRealFreeSpace(t *testing.T) {
	m := &Movers{
		statFunc: func(path string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 95, FreeBytes: 5}, nil
		},
	}
	entry := planner.MoveEntry{Item: model.MediaItem{Title: "Big Movie", SizeBytes: 50}, ToPath: "/cold"}

	if err := m.verifyRoom(entry); err == nil {
		t.Fatal("expected verifyRoom to refuse a move whose destination doesn't actually have room")
	}
}

func TestVerifyRoom_AllowsWhenRealFreeSpaceIsSufficient(t *testing.T) {
	m := &Movers{
		statFunc: func(path string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 40, FreeBytes: 60}, nil
		},
	}
	entry := planner.MoveEntry{Item: model.MediaItem{Title: "Small Movie", SizeBytes: 50}, ToPath: "/cold"}

	if err := m.verifyRoom(entry); err != nil {
		t.Fatalf("expected verifyRoom to allow a move with sufficient real free space, got %v", err)
	}
}

func TestVerifyRoom_RejectsWhenMoveWouldExceedDestinationCeiling(t *testing.T) {
	m := &Movers{
		statFunc: func(path string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 80, FreeBytes: 20}, nil
		},
	}
	entry := planner.MoveEntry{
		Item:           model.MediaItem{Title: "Ceiling Breaker", SizeBytes: 10},
		ToPath:         "/hot",
		MaxUsedPercent: 85,
	}

	err := m.verifyRoom(entry)
	if err == nil {
		t.Fatal("expected verifyRoom to reject a move that has raw room but would exceed the destination's hard ceiling")
	}
	if !strings.Contains(err.Error(), "90.0% used") || !strings.Contains(err.Error(), "85.0% hard ceiling") {
		t.Fatalf("expected the failure to report projected usage and the hard ceiling, got %v", err)
	}
}

// TestVerifyRoom_SkipsCheckWhenSizeUnknown documents the deliberate
// fail-open behavior for the one case verifyRoom can't meaningfully
// judge: an item with no recorded size at all. Refusing every such move
// would turn a data gap into a blanket outage; there's nothing to check
// against, so it defers to whatever upstream already decided.
func TestVerifyRoom_SkipsCheckWhenSizeUnknown(t *testing.T) {
	m := &Movers{
		statFunc: func(path string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 99, FreeBytes: 1}, nil
		},
	}
	entry := planner.MoveEntry{Item: model.MediaItem{Title: "Unknown Size"}, ToPath: "/cold"} // SizeBytes: 0

	if err := m.verifyRoom(entry); err != nil {
		t.Fatalf("expected verifyRoom not to block on an item with no usable size, got %v", err)
	}
}

// TestApply_RefusesMoveWhenRealFreeSpaceIsInsufficient proves the
// pre-flight capacity check is actually wired into Apply's real execution
// path, not just unit-tested in isolation: an entry whose destination
// doesn't actually have room right now is marked Failed and Radarr is
// never even asked to move it. This is the same class of gap as
// settle()'s post-move confirmation (see TestSettle_RejectsPathMatchButShortSize
// above) - trusting the plan's precomputed capacity math instead of
// reality - but at the moment just before a move starts rather than after.
func TestApply_RefusesMoveWhenRealFreeSpaceIsInsufficient(t *testing.T) {
	statFunc := func(path string) (diskusage.Usage, error) {
		return diskusage.Usage{TotalBytes: 100, UsedBytes: 99, FreeBytes: 1}, nil
	}

	var mu sync.Mutex
	moveAttempted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/api/v3/movie/editor" {
			mu.Lock()
			moveAttempted = true
			mu.Unlock()
		}
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
			{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Too Big", SizeBytes: 50}, ToTier: "cold", ToPath: "/cold"},
		},
	}

	progress := m.Apply(plan, nil)
	progress.Wait()

	mu.Lock()
	attempted := moveAttempted
	mu.Unlock()
	if attempted {
		t.Fatal("expected Radarr to never be asked to move an item that doesn't actually fit")
	}

	snap := progress.Snapshot()
	if failed := snap.Failed(); len(failed) != 1 {
		t.Fatalf("expected the entry to be marked failed, got %+v", snap.Entries)
	}
	if moved := snap.Moved(); len(moved) != 0 {
		t.Fatalf("expected nothing moved, got %d", len(moved))
	}
}

// TestApply_LaterSameVolumeEntryFailsWithoutBlockingEarlierSuccess proves
// the pre-flight check is re-evaluated fresh for each entry in a
// same-volume sequential group, not just once for the whole group: the
// first item has real room and succeeds; by the time the second item's
// turn comes up, real free space has dropped below what it needs (drift
// between the plan's snapshot and reality), and it's refused - without
// that failure retroactively affecting the first item's already-recorded
// success. Partial progress within a volume group must survive a later
// entry in that same group failing its pre-flight check.
func TestApply_LaterSameVolumeEntryFailsWithoutBlockingEarlierSuccess(t *testing.T) {
	var mu sync.Mutex
	var callOrder []string

	// /cold has enough free room (60) for either single item (50) - but
	// not both. By the time item2's pre-flight check runs (strictly
	// after item1's move has been recorded), real free space has already
	// dropped to 10.
	statFunc := func(path string) (diskusage.Usage, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(callOrder) == 0 {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 40, FreeBytes: 60}, nil
		}
		return diskusage.Usage{TotalBytes: 100, UsedBytes: 90, FreeBytes: 10}, nil
	}

	srv := fakeRadarrMoveServer(t, &mu, &callOrder)
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
			{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Item1", SizeBytes: 50}, ToTier: "cold", ToPath: "/cold"},
			{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Item2", SizeBytes: 50}, ToTier: "cold", ToPath: "/cold"},
		},
	}

	progress := m.Apply(plan, nil)
	progress.Wait()

	snap := progress.Snapshot()
	if moved := snap.Moved(); len(moved) != 1 || moved[0].Entry.Item.ID != 1 {
		t.Fatalf("expected exactly item1 moved, got %+v", snap.Entries)
	}
	if failed := snap.Failed(); len(failed) != 1 || failed[0].Entry.Item.ID != 2 {
		t.Fatalf("expected exactly item2 failed, got %+v", snap.Entries)
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

	srv := fakeRadarrMoveServer(t, &mu, &callOrder)
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

	waitFor(t, func() bool {
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

	srv := fakeRadarrMoveServer(t, &mu, &callOrder)
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

	waitFor(t, func() bool {
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

	srv := fakeRadarrMoveServer(t, &mu, &callOrder)
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

	waitFor(t, func() bool {
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
	waitFor(t, func() bool {
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
	waitFor(t, func() bool {
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
	if i0 < 0 || i1 < 0 || i2 < 0 || i0 >= i1 || i1 >= i2 {
		t.Fatalf("expected strict phase order 0 < 1 < 2, got %v", finalCalls)
	}
}

// TestApply_FailedPhaseBlocksLaterPhases covers the failure half of phase
// ordering: waiting for an earlier phase to finish is not enough. Every
// physical move in it must land, because a later phase may only fit after
// the earlier moves change the real disk usage. Here Radarr rejects phase
// 0, so phase 1 must never be sent to Radarr at all.
func TestApply_FailedPhaseBlocksLaterPhases(t *testing.T) {
	var mu sync.Mutex
	var moveIDs []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v3/movie/editor" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var req struct {
			MovieIDs []int `json:"movieIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		moveIDs = append(moveIDs, req.MovieIDs...)
		mu.Unlock()

		if len(req.MovieIDs) > 0 && req.MovieIDs[0] == 1 {
			http.Error(w, "move rejected", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hist, err := history.Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}

	m := &Movers{
		Radarr:  arrapi.NewRadarrClient(srv.URL, "key"),
		History: hist,
		statFunc: func(path string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 10, FreeBytes: 90}, nil
		},
	}
	plan := &planner.Plan{Entries: []planner.MoveEntry{
		{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Failed prerequisite", SizeBytes: 10}, ToPath: "/hot", Phase: 0},
		{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Dependent fill", SizeBytes: 10}, ToPath: "/cold", Phase: 1},
	}}

	progress := m.Apply(plan, nil)
	progress.Wait()

	mu.Lock()
	gotMoveIDs := append([]int(nil), moveIDs...)
	mu.Unlock()
	if len(gotMoveIDs) != 1 || gotMoveIDs[0] != 1 {
		t.Fatalf("expected only phase 0 to be sent to Radarr, got move IDs %v", gotMoveIDs)
	}

	snap := progress.Snapshot()
	if snap.Entries[0].Status != StatusFailed {
		t.Fatalf("expected rejected phase-0 move to fail, got %+v", snap.Entries[0])
	}
	if snap.Entries[1].Status != StatusFailed || !strings.Contains(snap.Entries[1].Err, "not attempted: prerequisite phase 0 failed") {
		t.Fatalf("expected phase-1 move to be marked not attempted because its prerequisite failed, got %+v", snap.Entries[1])
	}
}

// TestApply_AmbiguousCommandErrorStopsDestinationQueue covers a lost Arr
// response after the complete editor request was received. Coldarr cannot
// know whether Arr started the transfer before the connection disappeared,
// so it must block both the rest of that destination volume's queue and
// phases whose capacity assumptions depend on this move.
func TestApply_AmbiguousCommandErrorStopsDestinationQueue(t *testing.T) {
	var mu sync.Mutex
	var moveIDs []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v3/movie/editor" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var req struct {
			MovieIDs []int `json:"movieIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		moveIDs = append(moveIDs, req.MovieIDs...)
		mu.Unlock()

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("test server does not support connection hijacking")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijacking response connection: %v", err)
			return
		}
		_ = conn.Close() // lose the response after the request was received
	}))
	defer srv.Close()

	hist, err := history.Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}
	m := &Movers{
		Radarr:  arrapi.NewRadarrClient(srv.URL, "key"),
		History: hist,
		statFunc: func(path string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 10, FreeBytes: 90}, nil
		},
	}
	plan := &planner.Plan{Entries: []planner.MoveEntry{
		{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Ambiguous command", SizeBytes: 10}, ToPath: "/shared", Phase: 0},
		{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Same-volume follower", SizeBytes: 10}, ToPath: "/shared", Phase: 0},
		{Item: model.MediaItem{ArrApp: "radarr", ID: 3, Title: "Dependent phase", SizeBytes: 10}, ToPath: "/other", Phase: 1},
	}}

	progress := m.Apply(plan, nil)
	progress.Wait()

	mu.Lock()
	gotMoveIDs := append([]int(nil), moveIDs...)
	mu.Unlock()
	if len(gotMoveIDs) != 1 || gotMoveIDs[0] != 1 {
		t.Fatalf("expected only the ambiguous first command to reach Radarr, got move IDs %v", gotMoveIDs)
	}

	snap := progress.Snapshot()
	if snap.Entries[0].Status != StatusFailed {
		t.Fatalf("expected the command with a lost response to fail visibly, got %+v", snap.Entries[0])
	}
	if snap.Entries[1].Status != StatusFailed || !strings.Contains(snap.Entries[1].Err, "may still be running") {
		t.Fatalf("expected the ambiguous command to stop its destination queue, got %+v", snap.Entries[1])
	}
	if snap.Entries[2].Status != StatusFailed || !strings.Contains(snap.Entries[2].Err, "not attempted: prerequisite phase 0 failed") {
		t.Fatalf("expected the ambiguous command to block dependent phases, got %+v", snap.Entries[2])
	}
}

// TestApply_HistoryFailureDoesNotBlockDependentPhase distinguishes a
// post-landing bookkeeping failure from an unmet physical prerequisite.
// Phase 0 is confirmed at its destination but cannot persist history, so
// it remains visibly Failed. Its capacity change is nevertheless real,
// and phase 1 must be allowed to execute.
func TestApply_HistoryFailureDoesNotBlockDependentPhase(t *testing.T) {
	var mu sync.Mutex
	var callOrder []string
	srv := fakeRadarrMoveServer(t, &mu, &callOrder)
	defer srv.Close()

	// Load succeeds while the parent is absent, then a regular file at
	// that parent path makes the first Append's MkdirAll fail. Once phase
	// 0 reports that failure, replace it with a directory so phase 1 can
	// demonstrate a normal successful completion.
	historyParent := t.TempDir() + "/history-parent"
	hist, err := history.Load(historyParent + "/history.json")
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}
	if err := os.WriteFile(historyParent, []byte("blocks history directory"), 0o600); err != nil {
		t.Fatalf("creating history blocker: %v", err)
	}

	phase1Gate := make(chan struct{})
	var releaseOnce sync.Once
	releasePhase1 := func() { releaseOnce.Do(func() { close(phase1Gate) }) }
	defer releasePhase1()

	m := &Movers{
		Radarr:              arrapi.NewRadarrClient(srv.URL, "key"),
		History:             hist,
		SettleCheckInterval: time.Millisecond,
		SettleStableChecks:  1,
		statFunc: func(path string) (diskusage.Usage, error) {
			if path == "/phase1" {
				<-phase1Gate
			}
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 10, FreeBytes: 90}, nil
		},
	}
	plan := &planner.Plan{Entries: []planner.MoveEntry{
		{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Landed, history failed", SizeBytes: 10}, ToPath: "/phase0", Phase: 0},
		{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Capacity dependent", SizeBytes: 10}, ToPath: "/phase1", Phase: 1},
	}}

	progress := m.Apply(plan, nil)
	waitFor(t, func() bool {
		return progress.Snapshot().Entries[0].Status == StatusFailed
	})

	first := progress.Snapshot().Entries[0]
	if !strings.Contains(first.Err, "moved, but recording history failed") {
		t.Fatalf("expected phase 0's history failure to remain visible, got %+v", first)
	}
	if err := os.Remove(historyParent); err != nil {
		t.Fatalf("removing history blocker: %v", err)
	}
	if err := os.Mkdir(historyParent, 0o750); err != nil {
		t.Fatalf("creating history directory: %v", err)
	}
	releasePhase1()
	progress.Wait()

	snap := progress.Snapshot()
	if snap.Entries[0].Status != StatusFailed {
		t.Fatalf("expected phase 0 to retain its visible history failure, got %+v", snap.Entries[0])
	}
	if snap.Entries[1].Status != StatusDone {
		t.Fatalf("expected landed phase 0 to unlock phase 1 despite its history failure, got %+v", snap.Entries[1])
	}

	mu.Lock()
	gotCalls := append([]string(nil), callOrder...)
	mu.Unlock()
	if i0, i1 := indexOf(gotCalls, "1@/phase0"), indexOf(gotCalls, "2@/phase1"); i0 < 0 || i1 <= i0 {
		t.Fatalf("expected both phases to execute in order, got %v", gotCalls)
	}
}

// TestApply_SettleStatFailureBlocksDependentPhase proves that accepting
// an Arr command is not success by itself. If the mover loses the ability
// to observe the destination immediately afterward, the first entry is
// reported as accepted-but-unconfirmed; neither the rest of its physical
// destination queue nor a dependent phase may be started.
func TestApply_SettleStatFailureBlocksDependentPhase(t *testing.T) {
	var mu sync.Mutex
	var moveIDs []int
	statCalls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v3/movie/editor" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var req struct {
			MovieIDs []int `json:"movieIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		moveIDs = append(moveIDs, req.MovieIDs...)
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
		statFunc: func(path string) (diskusage.Usage, error) {
			mu.Lock()
			defer mu.Unlock()
			statCalls++
			if statCalls <= 2 {
				// verifyRoom before Radarr accepts the move, then
				// settle's initial baseline immediately afterward.
				return diskusage.Usage{TotalBytes: 100, UsedBytes: 10, FreeBytes: 90}, nil
			}
			// The first polling stat during settle fails.
			return diskusage.Usage{}, fmt.Errorf("destination unavailable")
		},
	}
	plan := &planner.Plan{Entries: []planner.MoveEntry{
		{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Unobservable move", SizeBytes: 10}, ToPath: "/hot", Phase: 0},
		{Item: model.MediaItem{ArrApp: "radarr", ID: 3, Title: "Same-volume follower", SizeBytes: 10}, ToPath: "/hot", Phase: 0},
		{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Dependent fill", SizeBytes: 10}, ToPath: "/cold", Phase: 1},
	}}

	progress := m.Apply(plan, nil)
	progress.Wait()

	mu.Lock()
	gotMoveIDs := append([]int(nil), moveIDs...)
	mu.Unlock()
	if len(gotMoveIDs) != 1 || gotMoveIDs[0] != 1 {
		t.Fatalf("expected only the accepted phase-0 command, got move IDs %v", gotMoveIDs)
	}

	snap := progress.Snapshot()
	if snap.Entries[0].Status != StatusFailed || !strings.Contains(snap.Entries[0].Err, "move command was accepted") || !strings.Contains(snap.Entries[0].Err, "destination unavailable") {
		t.Fatalf("expected an explicit accepted-but-unconfirmed stat failure, got %+v", snap.Entries[0])
	}
	if snap.Entries[1].Status != StatusFailed || !strings.Contains(snap.Entries[1].Err, "may still be running") {
		t.Fatalf("expected the same-volume queue to stop after an unconfirmed transfer, got %+v", snap.Entries[1])
	}
	if snap.Entries[2].Status != StatusFailed || !strings.Contains(snap.Entries[2].Err, "not attempted: prerequisite phase 0 failed") {
		t.Fatalf("expected dependent phase to be marked not attempted, got %+v", snap.Entries[2])
	}
	if got := hist.All(); len(got) != 0 {
		t.Fatalf("an unconfirmed move must not be recorded in history, got %+v", got)
	}
}

// TestApply_UnconfirmedSettleTimeoutBlocksDependentPhase exercises the
// other settle failure mode: disk stats remain readable, but Radarr never
// reports the item at its destination. Exhausting SettleMaxWait must be a
// visible failure, not an implicit success that unlocks the next phase.
func TestApply_UnconfirmedSettleTimeoutBlocksDependentPhase(t *testing.T) {
	var mu sync.Mutex
	var moveIDs []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v3/movie/editor":
			var req struct {
				MovieIDs []int `json:"movieIds"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			moveIDs = append(moveIDs, req.MovieIDs...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": "completed"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/command/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "status": "completed"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie/1":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "sizeOnDisk": 10, "path": "/still/on/source"})
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
		SettleStableChecks:  1,
		SettleMaxWait:       5 * time.Millisecond,
		statFunc: func(path string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 10, FreeBytes: 90}, nil
		},
	}
	plan := &planner.Plan{Entries: []planner.MoveEntry{
		{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Never confirmed", SizeBytes: 10}, ToPath: "/hot", Phase: 0},
		{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Dependent fill", SizeBytes: 10}, ToPath: "/cold", Phase: 1},
	}}

	progress := m.Apply(plan, nil)
	progress.Wait()

	mu.Lock()
	gotMoveIDs := append([]int(nil), moveIDs...)
	mu.Unlock()
	if len(gotMoveIDs) != 1 || gotMoveIDs[0] != 1 {
		t.Fatalf("expected only phase 0 to be sent to Radarr, got move IDs %v", gotMoveIDs)
	}

	snap := progress.Snapshot()
	if snap.Entries[0].Status != StatusFailed || !strings.Contains(snap.Entries[0].Err, "could not be confirmed within") {
		t.Fatalf("expected an explicit settle-timeout failure, got %+v", snap.Entries[0])
	}
	if snap.Entries[1].Status != StatusFailed || !strings.Contains(snap.Entries[1].Err, "not attempted: prerequisite phase 0 failed") {
		t.Fatalf("expected dependent phase to be marked not attempted, got %+v", snap.Entries[1])
	}
	if got := hist.All(); len(got) != 0 {
		t.Fatalf("an unconfirmed move must not be recorded in history, got %+v", got)
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
	waitFor(t, func() bool { return progress.Snapshot().Done })

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

// TestSettle_RejectsPathMatchButShortSize proves confirmLanded checks the
// rescanned size, not just the path. Disk usage here looks fully grown
// and stable from the very first check - the exact false-positive signal
// that let Coldarr previously declare a move "done" - and Radarr already
// reports the item's path as the destination too. But the file is still
// far short of its expected size (genuinely mid-copy), so the move must
// not be marked done until the reported size actually catches up.
func TestSettle_RejectsPathMatchButShortSize(t *testing.T) {
	const toPath = "/cold/movies"
	const wantSize = int64(1_000_000)

	statFunc := func(path string) (diskusage.Usage, error) {
		// Plenty of free space, so verifyRoom's pre-flight check (see
		// mover.go) lets the move proceed - this test is specifically
		// about settle()'s post-move confirmation, not the pre-flight
		// capacity check.
		return diskusage.Usage{TotalBytes: 10_000_000, UsedBytes: 5_000_000, FreeBytes: 5_000_000, UsedPercent: 50}, nil
	}

	var mu sync.Mutex
	reportedSize := int64(10) // far short of wantSize - still copying
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
			mu.Lock()
			size := reportedSize
			mu.Unlock()
			// Path already matches the destination even though the file
			// is still short - the exact scenario a path-only check
			// would have wrongly accepted.
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "sizeOnDisk": size, "path": toPath + "/Item1"})
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
			{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Item1", SizeBytes: wantSize}, ToTier: "cold", ToPath: toPath},
		},
	}

	progress := m.Apply(plan, nil)

	// Give it many settle-check intervals' worth of real time while the
	// reported size stays short - disk usage and path both already look
	// "correct" on every single check, so if either alone were trusted
	// this would already be marked done.
	time.Sleep(50 * time.Millisecond)
	if progress.Snapshot().Done {
		t.Fatal("must not settle while Radarr reports the file far short of its expected size")
	}
	mu.Lock()
	calls := rescanCalls
	mu.Unlock()
	if calls == 0 {
		t.Fatal("expected confirmLanded to actually have been checking via rescan")
	}

	mu.Lock()
	reportedSize = wantSize
	mu.Unlock()

	waitFor(t, func() bool { return progress.Snapshot().Done })

	snap := progress.Snapshot()
	if failed := snap.Failed(); len(failed) != 0 {
		t.Fatalf("expected no failures, got %+v", failed)
	}
	if moved := snap.Moved(); len(moved) != 1 {
		t.Fatalf("expected the item moved once its reported size actually matched, got %d", len(moved))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
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

// recordingReporter appends each report into the same ordered log the fake
// Radarr server writes its move calls to, so a test can assert where a
// report falls relative to the moves around it.
type recordingReporter struct {
	mu    *sync.Mutex
	order *[]string
	err   error
}

func (r *recordingReporter) ReportMoved(oldPath, newPath string) error {
	r.mu.Lock()
	*r.order = append(*r.order, fmt.Sprintf("report:%s -> %s", oldPath, newPath))
	r.mu.Unlock()
	return r.err
}

// TestApply_ReportsEachMoveAsItLands is the timing guarantee the reporter
// exists for. A consumer's indexing is slow and starts late - Jellyfin
// sits on a reported path for a minute before touching it, then
// revalidates the whole containing library root - so what matters is not
// merely that every move gets reported, but that each one is handed over
// while there are still moves left for that work to overlap. Interleaving
// is what proves it: item 1's report lands before item 2's move is even
// issued, which a batch built up and delivered at the end cannot do.
func TestApply_ReportsEachMoveAsItLands(t *testing.T) {
	var mu sync.Mutex
	var callOrder []string

	srv := fakeRadarrMoveServer(t, &mu, &callOrder)
	defer srv.Close()

	hist, err := history.Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}

	m := &Movers{
		Radarr:              arrapi.NewRadarrClient(srv.URL, "key"),
		History:             hist,
		Reporter:            &recordingReporter{mu: &mu, order: &callOrder},
		Logf:                t.Logf,
		SettleCheckInterval: time.Millisecond,
		SettleStableChecks:  1,
		statFunc: func(path string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 50, FreeBytes: 50, UsedPercent: 50}, nil
		},
	}

	// Both entries target the same destination, so they're serialized and
	// the interleaving asserted below is deterministic.
	plan := &planner.Plan{
		Entries: []planner.MoveEntry{
			{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Movie 1", Path: "/hot/Movie 1"}, ToTier: "cold", ToPath: "/cold"},
			{Item: model.MediaItem{ArrApp: "radarr", ID: 2, Title: "Movie 2", Path: "/hot/Movie 2"}, ToTier: "cold", ToPath: "/cold"},
		},
	}

	progress := m.Apply(plan, nil)
	progress.Wait()

	mu.Lock()
	got := append([]string(nil), callOrder...)
	mu.Unlock()

	want := []string{
		"1@/cold",
		"report:/hot/Movie 1 -> /cold",
		"2@/cold",
		"report:/hot/Movie 2 -> /cold",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}

	for _, e := range progress.Snapshot().Moved() {
		if !e.Reported {
			t.Errorf("%s: Reported = false, want true so the end-of-run pass doesn't report it a second time", e.Entry.Item.Title)
		}
	}
}

// TestApply_ReporterFailureLeavesTheMoveUnreported: a downstream consumer
// being unreachable says nothing about whether the media moved, which
// Radarr already confirmed, so the entry still completes. But it must not
// claim to have been reported - that flag is exactly what the end-of-run
// pass uses to pick out the items still needing one.
func TestApply_ReporterFailureLeavesTheMoveUnreported(t *testing.T) {
	var mu sync.Mutex
	var callOrder []string

	srv := fakeRadarrMoveServer(t, &mu, &callOrder)
	defer srv.Close()

	hist, err := history.Load(t.TempDir() + "/history.json")
	if err != nil {
		t.Fatalf("history.Load: %v", err)
	}

	m := &Movers{
		Radarr:              arrapi.NewRadarrClient(srv.URL, "key"),
		History:             hist,
		Reporter:            &recordingReporter{mu: &mu, order: &callOrder, err: errors.New("jellyfin unreachable")},
		Logf:                t.Logf,
		SettleCheckInterval: time.Millisecond,
		SettleStableChecks:  1,
		statFunc: func(path string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 100, UsedBytes: 50, FreeBytes: 50, UsedPercent: 50}, nil
		},
	}

	plan := &planner.Plan{
		Entries: []planner.MoveEntry{
			{Item: model.MediaItem{ArrApp: "radarr", ID: 1, Title: "Movie 1", Path: "/hot/Movie 1"}, ToTier: "cold", ToPath: "/cold"},
		},
	}

	progress := m.Apply(plan, nil)
	progress.Wait()

	moved := progress.Snapshot().Moved()
	if len(moved) != 1 {
		t.Fatalf("expected the move to complete despite the reporter failing, got %+v", progress.Snapshot().Entries)
	}
	if moved[0].LandedPath != "/cold" {
		t.Errorf("LandedPath = %q, want /cold", moved[0].LandedPath)
	}
	if moved[0].Reported {
		t.Error("Reported = true after the report failed, which would make the end-of-run pass skip the one item that still needs reporting")
	}
}
