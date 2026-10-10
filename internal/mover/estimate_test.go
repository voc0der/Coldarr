package mover

import (
	"errors"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/planner"
)

func TestEstimate(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	entry := func(size int64, volume string, phase int, status MoveStatus, elapsed time.Duration) EntryProgress {
		e := EntryProgress{
			Entry:  planner.MoveEntry{Item: model.MediaItem{SizeBytes: size}, Phase: phase},
			Status: status, volume: volume,
		}
		if status != StatusPending {
			e.Started = now.Add(-elapsed)
		}
		if status == StatusDone || status == StatusFailed {
			e.Finished = now
		}
		return e
	}
	// These fixtures use arbitrary byte units so that the critical path is
	// easy to check by hand: volume A moves 10 units/s, B moves 5 units/s.
	aSample := entry(100, "a", 0, StatusDone, 10*time.Second)
	bSample := entry(100, "b", 0, StatusDone, 20*time.Second)
	tests := []struct {
		name      string
		entries   []EntryProgress
		percent   int
		remaining time.Duration
		known     bool
		overdue   bool
	}{
		{name: "empty snapshot"},
		{name: "first large transfer has no sample", entries: []EntryProgress{entry(1000, "a", 0, StatusMoving, time.Minute)}},
		{name: "sizes outweigh item counts", entries: []EntryProgress{aSample, entry(900, "a", 0, StatusPending, 0)}, percent: 10, remaining: 90 * time.Second, known: true},
		{name: "active elapsed deducted", entries: []EntryProgress{aSample, entry(100, "a", 0, StatusMoving, 4*time.Second)}, percent: 50, remaining: 6 * time.Second, known: true},
		{name: "same volume queue is serial", entries: []EntryProgress{aSample, entry(100, "a", 0, StatusMoving, 4*time.Second), entry(200, "a", 0, StatusPending, 0)}, percent: 25, remaining: 26 * time.Second, known: true},
		{name: "parallel queues use slowest drive", entries: []EntryProgress{aSample, bSample, entry(200, "a", 0, StatusPending, 0), entry(200, "b", 0, StatusPending, 0)}, percent: 33, remaining: 40 * time.Second, known: true},
		{name: "dependent phases add", entries: []EntryProgress{aSample, bSample, entry(200, "a", 0, StatusPending, 0), entry(200, "b", 1, StatusPending, 0)}, percent: 33, remaining: 60 * time.Second, known: true},
		{name: "new drive borrows pooled pace", entries: []EntryProgress{aSample, bSample, entry(200, "c", 0, StatusPending, 0)}, percent: 50, remaining: 30 * time.Second, known: true},
		{name: "failures resolve work without training pace", entries: []EntryProgress{aSample, entry(800, "a", 0, StatusFailed, time.Millisecond), entry(100, "a", 0, StatusPending, 0)}, percent: 90, remaining: 10 * time.Second, known: true},
		{name: "only failed samples cannot predict", entries: []EntryProgress{entry(100, "a", 0, StatusFailed, time.Second), entry(100, "a", 0, StatusPending, 0)}, percent: 50},
		{name: "missing sizes use observed item duration", entries: []EntryProgress{entry(0, "a", 0, StatusDone, 10*time.Second), entry(0, "a", 0, StatusPending, 0)}, percent: 50, remaining: 10 * time.Second, known: true},
		{name: "mixed sizes keep unknown work visible", entries: []EntryProgress{aSample, entry(0, "a", 0, StatusPending, 0)}, percent: 50, remaining: 10 * time.Second, known: true},
		{name: "invalid sizes are treated as unknown", entries: []EntryProgress{aSample, entry(-100, "a", 0, StatusPending, 0)}, percent: 50, remaining: 10 * time.Second, known: true},
		{name: "overdue transfer cannot promise zero", entries: []EntryProgress{aSample, entry(100, "a", 0, StatusMoving, time.Minute)}, percent: 50, overdue: true},
		{name: "overdue parallel drive invalidates entire ETA", entries: []EntryProgress{aSample, bSample, entry(100, "a", 0, StatusMoving, time.Minute), entry(1000, "b", 0, StatusPending, 0)}, percent: 15, overdue: true},
		{name: "pending completion never rounds to 100", entries: []EntryProgress{aSample, entry(0, "a", 0, StatusFailed, 0)}, percent: 99, known: true},
		{name: "zero duration is not a sample", entries: []EntryProgress{entry(100, "a", 0, StatusDone, 0), entry(100, "a", 0, StatusPending, 0)}, percent: 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := ProgressSnapshot{Started: now.Add(-time.Hour), Entries: tt.entries}
			got := snap.Estimate(now)
			if got.Percent != tt.percent || got.Remaining != tt.remaining || got.Known != tt.known || got.Overdue != tt.overdue || got.Elapsed != time.Hour {
				t.Fatalf("Estimate = %+v; want %d%%, %v remaining, known=%v, overdue=%v, elapsed=1h", got, tt.percent, tt.remaining, tt.known, tt.overdue)
			}
		})
	}
}

func TestEstimateFinishedAndClockBounds(t *testing.T) {
	now := time.Now()
	snap := ProgressSnapshot{Started: now.Add(-time.Hour), Finished: now.Add(-time.Minute), Done: true}
	if got := snap.Estimate(now); got.Percent != 100 || got.Remaining != 0 || !got.Known || got.Elapsed != 59*time.Minute {
		t.Fatalf("finished estimate = %+v; want 100%% and frozen elapsed time", got)
	}
	snap = ProgressSnapshot{Started: now.Add(time.Minute)}
	if got := snap.Estimate(now); got.Elapsed != 0 {
		t.Fatalf("future start gives negative elapsed: %+v", got)
	}
	if got := (ProgressSnapshot{}).Estimate(now); got.Elapsed != 0 {
		t.Fatalf("unknown start gives nonzero elapsed: %+v", got)
	}
}

func TestProgressTimingAndSharedVolumeEstimate(t *testing.T) {
	plan := &planner.Plan{Entries: []planner.MoveEntry{
		{Item: model.MediaItem{SizeBytes: 100}, ToPath: "/cold/a"},
		{Item: model.MediaItem{SizeBytes: 100}, ToPath: "/cold/b"},
		{Item: model.MediaItem{SizeBytes: 100}, ToPath: "/cold/a"},
	}}
	p := newProgress(plan, map[string]uint64{"/cold/a": 42, "/cold/b": 42})
	p.setStatus(0, StatusMoving, nil)
	moving := p.Snapshot()
	if moving.Entries[0].Started.Before(moving.Started) || !moving.Entries[0].Finished.IsZero() {
		t.Fatalf("bad moving timestamps: %+v", moving.Entries[0])
	}
	p.setStatus(0, StatusDone, nil)
	snap := p.Snapshot()
	if snap.Entries[0].Finished.Before(snap.Entries[0].Started) || !moving.Entries[0].Finished.IsZero() {
		t.Fatal("completion time missing or earlier snapshot mutated")
	}
	// Deterministic timings on the copy let us verify that different roots
	// on the same physical device are estimated as a single serial queue.
	now := time.Now()
	snap.Entries[0].Started = now.Add(-10 * time.Second)
	snap.Entries[0].Finished = now
	if got := snap.Estimate(now); got.Remaining != 20*time.Second {
		t.Fatalf("shared destination estimate = %+v, want 20s", got)
	}
	p.setStatus(1, StatusFailed, errors.New("not attempted"))
	failed := p.Snapshot().Entries[1]
	if !failed.Started.IsZero() || failed.Finished.IsZero() {
		t.Fatalf("skipped move must not acquire a start time: %+v", failed)
	}

	// Paths with unknown devices remain independent, just as in Apply.
	p = newProgress(plan, nil)
	snap = p.Snapshot()
	snap.Entries[0].Status = StatusDone
	snap.Entries[0].Started = now.Add(-10 * time.Second)
	snap.Entries[0].Finished = now
	if got := snap.Estimate(now); got.Remaining != 10*time.Second {
		t.Fatalf("independent destination estimate = %+v, want 10s", got)
	}
}
