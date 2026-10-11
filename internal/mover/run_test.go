package mover

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/diskusage"
)

// testEntry is a move as the shared engine sees it, for an executor that
// isn't Radarr/Sonarr.
type testEntry struct {
	name  string
	dest  string
	size  int64
	phase int
}

func (e testEntry) MoveTitle() string       { return e.name }
func (e testEntry) MoveSize() int64         { return e.size }
func (e testEntry) MoveDestination() string { return e.dest }
func (e testEntry) MovePhase() int          { return e.phase }
func (e testEntry) MoveCeiling() float64    { return 0 }

// testExecutor lands every move on its first confirmation, unless told
// otherwise per entry, and records how many moves overlapped.
type testExecutor struct {
	mu       sync.Mutex
	moved    []string
	inFlight atomic.Int32
	peak     atomic.Int32
	// moveErr, confirmErr and landedErr fail the named entry's step.
	moveErr    map[string]error
	rejected   bool
	confirmErr map[string]error
	landedErr  map[string]error
}

func (x *testExecutor) Move(e testEntry) error {
	if err := x.moveErr[e.name]; err != nil {
		return err
	}
	n := x.inFlight.Add(1)
	for {
		peak := x.peak.Load()
		if n <= peak || x.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	x.mu.Lock()
	x.moved = append(x.moved, e.name)
	x.mu.Unlock()
	return nil
}

func (x *testExecutor) Rejected(error) bool { return x.rejected }

func (x *testExecutor) ConfirmLanded(e testEntry) (bool, string, error) {
	if err := x.confirmErr[e.name]; err != nil {
		return false, "", err
	}
	time.Sleep(5 * time.Millisecond) // long enough for overlaps to show
	x.inFlight.Add(-1)
	return true, e.dest + "/" + e.name, nil
}

func (x *testExecutor) Landed(e testEntry, landedPath string) (bool, error) {
	return false, x.landedErr[e.name]
}

func fastSerialOptions() Options {
	return Options{
		SettleCheckInterval: time.Millisecond,
		SettleStableChecks:  1,
		SettleMaxWait:       time.Second,
		Serial:              true,
		statFunc: func(string) (diskusage.Usage, error) {
			return diskusage.Usage{TotalBytes: 1 << 40, FreeBytes: 1 << 40}, nil
		},
	}
}

// TestRun_SerialRunsOneMoveAtATime: a serial run never has two moves in
// flight, even bound for different volumes, and runs them in plan order.
func TestRun_SerialRunsOneMoveAtATime(t *testing.T) {
	entries := []testEntry{{name: "a", dest: "/seed1"}, {name: "b", dest: "/seed2"}, {name: "c", dest: "/seed3"}}

	serial := &testExecutor{}
	progress := Run[testEntry](serial, entries, nil, fastSerialOptions())
	progress.Wait()
	if peak := serial.peak.Load(); peak != 1 {
		t.Errorf("serial run had %d moves in flight at once, want 1", peak)
	}
	if got := strings.Join(serial.moved, ","); got != "a,b,c" {
		t.Errorf("serial order = %s, want the plan's order", got)
	}
	snap := progress.Snapshot()
	if len(snap.Moved()) != 3 || snap.Entries[1].LandedPath != "/seed2/b" {
		t.Errorf("snapshot = %+v", snap.Entries)
	}

}

// TestRun_FinalConfirmationFailureStopsWaiting: an executor that says the
// move itself failed ends the wait at once instead of polling until the
// settle deadline - and, the destination's state now unknown, nothing
// else queued for it starts.
func TestRun_FinalConfirmationFailureStopsWaiting(t *testing.T) {
	x := &testExecutor{confirmErr: map[string]error{"a": fmt.Errorf("%w: qBittorrent reports missingFiles", ErrMoveFailed)}}
	opts := fastSerialOptions()
	opts.SettleMaxWait = time.Hour

	start := time.Now()
	progress := Run[testEntry](x, []testEntry{{name: "a", dest: "/seed"}, {name: "b", dest: "/seed"}}, nil, opts)
	progress.Wait()
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("run took %v, want it to stop at the final failure", elapsed)
	}
	snap := progress.Snapshot()
	if e := snap.Entries[0]; e.Status != StatusFailed || !strings.Contains(e.Err, "missingFiles") {
		t.Errorf("a = %+v, want the final failure", e)
	}
	if e := snap.Entries[1]; e.Status != StatusFailed || !strings.Contains(e.Err, "not attempted") {
		t.Errorf("b = %+v, want it never started", e)
	}

	// A plain confirmation error is retried until the deadline, and named
	// when it runs out.
	x = &testExecutor{confirmErr: map[string]error{"a": errors.New("still checking")}}
	opts.SettleMaxWait = 30 * time.Millisecond
	progress = Run[testEntry](x, []testEntry{{name: "a", dest: "/seed"}}, nil, opts)
	progress.Wait()
	if e := progress.Snapshot().Entries[0]; !strings.Contains(e.Err, "could not be confirmed") || !strings.Contains(e.Err, "still checking") {
		t.Errorf("a = %+v, want the deadline with the last confirmation error", e)
	}
}

// TestRun_AfterLandingFailureStillCountsAsLanded: a move whose follow-up
// fails (relabeling, recording history) is shown failed, but its capacity
// change happened, so the next phase still runs.
func TestRun_AfterLandingFailureStillCountsAsLanded(t *testing.T) {
	x := &testExecutor{landedErr: map[string]error{"a": errors.New("moved, but relabeling failed")}}
	progress := Run[testEntry](x, []testEntry{{name: "a", dest: "/seed"}, {name: "b", dest: "/seed", phase: 1}}, nil, fastSerialOptions())
	progress.Wait()
	snap := progress.Snapshot()
	if e := snap.Entries[0]; e.Status != StatusFailed || e.LandedPath == "" {
		t.Errorf("a = %+v, want failed but landed", e)
	}
	if e := snap.Entries[1]; e.Status != StatusDone {
		t.Errorf("b = %+v, want the next phase run", e)
	}
}

// TestRun_RejectedMoveLeavesTheQueueRunning: a request the app refused
// outright can't be writing anything, so the rest of the queue carries on;
// an ambiguous failure stops it.
func TestRun_RejectedMoveLeavesTheQueueRunning(t *testing.T) {
	for _, rejected := range []bool{true, false} {
		x := &testExecutor{moveErr: map[string]error{"a": errors.New("409 cannot create save path")}, rejected: rejected}
		progress := Run[testEntry](x, []testEntry{{name: "a", dest: "/seed"}, {name: "b", dest: "/seed"}}, nil, fastSerialOptions())
		progress.Wait()
		b := progress.Snapshot().Entries[1]
		if rejected && b.Status != StatusDone {
			t.Errorf("after a rejection, b = %+v, want it moved", b)
		}
		if !rejected && (b.Status != StatusFailed || !strings.Contains(b.Err, "not attempted")) {
			t.Errorf("after an ambiguous failure, b = %+v, want it never started", b)
		}
	}
}
