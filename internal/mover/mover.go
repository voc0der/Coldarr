// Package mover executes a planner.Plan by asking Radarr/Sonarr to relocate
// items - Coldarr never touches media files directly, so the owning Arr
// app's database stays the source of truth.
//
// Moves are serialized per destination physical volume: never more than
// one move in flight against the same disk at a time, though different
// volumes proceed concurrently. Firing every move in a plan at once, all
// at the same instant, is what caused a real deployment's storage
// subsystem to hang badly enough to take the whole host down - a burst of
// simultaneous large writes across several files/drives is a very
// different load profile than steady, one-at-a-time movement. Hot storage
// (a read source, not a write target) isn't throttled at all.
//
// A plan can also contain moves that free cold capacity (e.g. reclaiming a
// grow-risk item back to hot) alongside moves that consume it (packing a
// hot-eligible item onto cold). Since cold tiers routinely run within
// fractions of a percent of their ceiling, writing before freeing can fail
// outright - so every freeing move is run to completion, settled, before
// any filling move begins, regardless of which volumes they land on. See
// Apply.
package mover

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/planner"
)

type Movers struct {
	Radarr  *arrapi.RadarrClient
	Sonarr  *arrapi.SonarrClient
	History *history.Store

	// SettleCheckInterval, SettleStableChecks, and SettleMaxWait
	// control how long Apply waits, after asking Radarr/Sonarr to move
	// an item, for it to actually land on disk before starting the next
	// item queued for the same volume. Zero values fall back to
	// production defaults; tests override them to run fast.
	SettleCheckInterval time.Duration
	SettleStableChecks  int
	SettleMaxWait       time.Duration

	// statFunc defaults to diskusage.Stat; tests override it to observe
	// and control settling without touching a real filesystem.
	statFunc func(path string) (diskusage.Usage, error)
}

func (m *Movers) stat(path string) (diskusage.Usage, error) {
	if m.statFunc != nil {
		return m.statFunc(path)
	}
	return diskusage.Stat(path)
}

type MoveStatus string

const (
	StatusPending MoveStatus = "pending"
	StatusMoving  MoveStatus = "moving"
	StatusDone    MoveStatus = "done"
	StatusFailed  MoveStatus = "failed"
)

type EntryProgress struct {
	Entry  planner.MoveEntry
	Status MoveStatus
	Err    string
}

// ProgressSnapshot is a point-in-time, race-free copy of an apply run's
// state, safe to read from any goroutine (e.g. a web handler polling for
// status while the run continues in the background).
type ProgressSnapshot struct {
	Started  time.Time
	Finished time.Time
	Done     bool
	Entries  []EntryProgress
}

func (s ProgressSnapshot) Moved() []EntryProgress {
	return s.byStatus(StatusDone)
}

func (s ProgressSnapshot) Failed() []EntryProgress {
	return s.byStatus(StatusFailed)
}

func (s ProgressSnapshot) byStatus(status MoveStatus) []EntryProgress {
	var out []EntryProgress
	for _, e := range s.Entries {
		if e.Status == status {
			out = append(out, e)
		}
	}
	return out
}

// Progress is a live view of an in-progress (or completed) apply run.
type Progress struct {
	mu       sync.Mutex
	started  time.Time
	finished time.Time
	done     bool
	entries  []EntryProgress
	doneCh   chan struct{}
}

func newProgress(plan *planner.Plan) *Progress {
	entries := make([]EntryProgress, len(plan.Entries))
	for i, e := range plan.Entries {
		entries[i] = EntryProgress{Entry: e, Status: StatusPending}
	}
	return &Progress{started: time.Now(), entries: entries, doneCh: make(chan struct{})}
}

func (p *Progress) setStatus(i int, status MoveStatus, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[i].Status = status
	if err != nil {
		p.entries[i].Err = err.Error()
	}
}

func (p *Progress) markDone() {
	p.mu.Lock()
	p.done = true
	p.finished = time.Now()
	p.mu.Unlock()
	close(p.doneCh)
}

// Wait blocks until the apply run has finished - every volume's queue
// drained. For callers (like the CLI) that want to run synchronously.
func (p *Progress) Wait() {
	<-p.doneCh
}

// Snapshot returns a race-free copy of the current state.
func (p *Progress) Snapshot() ProgressSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := make([]EntryProgress, len(p.entries))
	copy(entries, p.entries)
	return ProgressSnapshot{Started: p.started, Finished: p.finished, Done: p.done, Entries: entries}
}

// Apply starts executing plan and returns immediately with a *Progress the
// caller can poll or Wait() on; the moves themselves continue in the
// background. volumeOf groups destination paths that are really the same
// physical volume (see internal/diskusage.DeviceID) - a path missing from
// it is treated as its own single-path volume, never merged with another
// path unless they're known for certain to share a device.
//
// Execution runs in two sequential phases so that any move freeing cold
// capacity (entry.FromRole == model.RoleCold) fully lands - including its
// settle - before any move consuming cold capacity (FromRole ==
// model.RoleHot) starts writing. See the package doc for why. Within each
// phase, moves are grouped and run exactly as before: serialized per
// destination volume, concurrent across different volumes.
func (m *Movers) Apply(plan *planner.Plan, volumeOf map[string]uint64) *Progress {
	progress := newProgress(plan)

	var freeing, filling []int
	for i, e := range plan.Entries {
		if e.FromRole == model.RoleCold {
			freeing = append(freeing, i)
		} else {
			filling = append(filling, i)
		}
	}

	go func() {
		m.runPhase(plan, freeing, progress, volumeOf)
		m.runPhase(plan, filling, progress, volumeOf)
		progress.markDone()
	}()

	return progress
}

// runPhase groups the given plan-entry indices by destination volume and
// runs each group's moves serially, with different volumes proceeding
// concurrently; it blocks until every move in this phase has finished
// (including settling).
func (m *Movers) runPhase(plan *planner.Plan, indices []int, progress *Progress, volumeOf map[string]uint64) {
	groups := map[string][]int{}
	var order []string
	for _, i := range indices {
		key := volumeKey(plan.Entries[i].ToPath, volumeOf)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}

	var wg sync.WaitGroup
	for _, key := range order {
		groupIndices := groups[key]
		wg.Add(1)
		go func(groupIndices []int) {
			defer wg.Done()
			for _, i := range groupIndices {
				m.runOne(plan.Entries[i], progress, i)
			}
		}(groupIndices)
	}
	wg.Wait()
}

func volumeKey(path string, volumeOf map[string]uint64) string {
	if dev, ok := volumeOf[path]; ok {
		return fmt.Sprintf("dev:%d", dev)
	}
	return "path:" + path
}

func (m *Movers) runOne(entry planner.MoveEntry, progress *Progress, idx int) {
	progress.setStatus(idx, StatusMoving, nil)

	var err error
	switch entry.Item.ArrApp {
	case "radarr":
		err = m.Radarr.MoveMovies([]int{entry.Item.ID}, entry.ToPath)
	case "sonarr":
		err = m.Sonarr.MoveSeries([]int{entry.Item.ID}, entry.ToPath)
	default:
		err = fmt.Errorf("unknown arr app %q", entry.Item.ArrApp)
	}
	if err != nil {
		progress.setStatus(idx, StatusFailed, err)
		return
	}

	m.settle(entry)

	if err := m.History.Append(history.Record{
		ArrApp:    entry.Item.ArrApp,
		ItemID:    entry.Item.ID,
		Title:     entry.Item.Title,
		FromTier:  entry.FromTier,
		FromPath:  entry.FromPath,
		ToTier:    entry.ToTier,
		ToPath:    entry.ToPath,
		SizeBytes: entry.Item.SizeBytes,
		MovedAt:   time.Now(),
	}); err != nil {
		progress.setStatus(idx, StatusFailed, fmt.Errorf("moved, but recording history failed: %w", err))
		return
	}

	progress.setStatus(idx, StatusDone, nil)
}

// settle waits for entry's move to actually land on the destination's
// filesystem before returning, so the next item queued for this same
// volume doesn't start writing concurrently with it. Radarr/Sonarr's move
// API returns once the operation is queued, not once the bytes are
// actually on disk, so this watches disk usage directly rather than
// trusting the API response alone - true regardless of exactly how
// Radarr/Sonarr implement the move internally.
//
// Disk-usage growth can't tell a finished move that added no measurable
// usage - the item was already sitting at (or very near) the destination,
// or landed via a same-volume rename - from one that simply hasn't
// started yet, so a move like that would otherwise wait out the full
// maxWait (hours, by default) every single time before ever being marked
// done, even though it's already sitting there correctly. Once growth
// looks stalled for a while, this also asks Radarr/Sonarr directly (via
// confirmLanded) whether it now considers the item moved - not on every
// check, since a rescan is comparatively expensive and unnecessary for
// the common case of a normal, growing transfer.
func (m *Movers) settle(entry planner.MoveEntry) {
	interval := m.SettleCheckInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	stableTarget := m.SettleStableChecks
	if stableTarget <= 0 {
		stableTarget = 3
	}
	maxWait := m.SettleMaxWait
	if maxWait <= 0 {
		maxWait = 6 * time.Hour
	}

	baseline, err := m.stat(entry.ToPath)
	if err != nil {
		// Can't observe it - don't block this volume's queue guessing
		// how long to wait; move on.
		return
	}

	tracker := newSettleTracker(baseline.UsedBytes, entry.Item.SizeBytes)
	deadline := time.Now().Add(maxWait)
	noGrowthChecks := 0

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		u, err := m.stat(entry.ToPath)
		if err != nil {
			return
		}
		if tracker.observe(u.UsedBytes, stableTarget) {
			return
		}

		if tracker.grown {
			noGrowthChecks = 0
			continue
		}
		noGrowthChecks++
		if noGrowthChecks >= stableTarget && m.confirmLanded(entry) {
			return
		}
	}
}

// confirmLanded asks Radarr/Sonarr to rescan the item's folder - forcing
// it to recompute the item's location/size from the real filesystem
// rather than a possibly-stale cached value - and reports whether it now
// considers the item's path to be under entry.ToPath. This is the
// ground-truth fallback settle() reaches for once disk-usage growth looks
// stalled; see settle's doc comment for why growth alone isn't enough.
func (m *Movers) confirmLanded(entry planner.MoveEntry) bool {
	switch entry.Item.ArrApp {
	case "radarr":
		if err := m.Radarr.RescanMovie(entry.Item.ID); err != nil {
			return false
		}
		_, path, found, err := m.Radarr.GetMovieSize(entry.Item.ID)
		return err == nil && found && isUnderPath(path, entry.ToPath)
	case "sonarr":
		if err := m.Sonarr.RescanSeries(entry.Item.ID); err != nil {
			return false
		}
		_, path, found, err := m.Sonarr.GetSeriesSize(entry.Item.ID)
		return err == nil && found && isUnderPath(path, entry.ToPath)
	default:
		return false
	}
}

// isUnderPath reports whether path is root itself or a descendant of it.
func isUnderPath(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// settleTracker decides whether a transfer looks complete from a series
// of disk-usage-used-bytes readings alone, with no knowledge of
// Radarr/Sonarr's internal state. It requires usage to have grown by
// roughly the item's expected size before it will ever consider the
// reading "stable" - otherwise a transfer that simply hasn't started yet
// (usage unchanged because nothing has happened) would look identical to
// one that's already finished.
type settleTracker struct {
	startUsed    uint64
	targetGrowth uint64
	lastUsed     uint64
	grown        bool
	stableCount  int
}

func newSettleTracker(startUsed uint64, itemSizeBytes int64) *settleTracker {
	var growth uint64
	if itemSizeBytes > 0 {
		growth = uint64(float64(itemSizeBytes) * 0.9)
	}
	return &settleTracker{startUsed: startUsed, targetGrowth: growth, lastUsed: startUsed}
}

// observe records a new usage reading and reports whether the transfer
// should now be considered settled.
func (t *settleTracker) observe(used uint64, stableTarget int) bool {
	if !t.grown && used >= t.startUsed+t.targetGrowth {
		t.grown = true
	}

	if used == t.lastUsed {
		if t.grown {
			t.stableCount++
			return t.stableCount >= stableTarget
		}
		return false
	}

	t.stableCount = 0
	t.lastUsed = used
	return false
}
