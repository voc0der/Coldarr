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
// A plan can also contain moves that free capacity (e.g. reclaiming a
// grow-risk item back to hot) that other moves in the same plan depend on
// - a fill might only be possible because an earlier reclaim freed cold
// room, and a reclaim might only be possible because an earlier fill freed
// hot room. The planner resolves this by stamping each entry with a Phase
// number reflecting real dependency order (see planner.MoveEntry.Phase),
// and Apply executes phases strictly in that order, waiting for one to
// fully land - including settling - before the next begins. See Apply.
package mover

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
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
// Execution runs in as many sequential phases as the plan actually needs,
// in ascending planner.MoveEntry.Phase order - a later phase never starts
// until every earlier phase has fully landed on disk (including
// settling), since the planner may only have found some later-phase
// entries a valid move because an earlier phase's entries free the space
// they need. Within a phase, moves are grouped and run exactly as before:
// serialized per destination volume, concurrent across different volumes.
func (m *Movers) Apply(plan *planner.Plan, volumeOf map[string]uint64) *Progress {
	progress := newProgress(plan)

	byPhase := map[int][]int{}
	for i, e := range plan.Entries {
		byPhase[e.Phase] = append(byPhase[e.Phase], i)
	}
	phases := make([]int, 0, len(byPhase))
	for p := range byPhase {
		phases = append(phases, p)
	}
	sort.Ints(phases)

	go func() {
		for _, p := range phases {
			m.runPhase(plan, byPhase[p], progress, volumeOf)
		}
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

// verifyRoom re-stats entry.ToPath immediately before firing the move and
// refuses to proceed if it doesn't actually have room for the item right
// now. The plan's own capacity math (see internal/planner) is computed
// once, from a snapshot taken before the run started - a long-running
// apply, or anything else writing to the same destination in the
// meantime (another download landing, a concurrent process, drift from
// an earlier bug), can leave real free space short of what the plan
// assumed by the time this specific entry's turn comes up. This is the
// last line of defense against ever handing Radarr/Sonarr a move with
// nowhere for it to actually land - independent of whether the plan (or
// anything upstream of it) got the arithmetic right.
func (m *Movers) verifyRoom(entry planner.MoveEntry) error {
	need := entry.Item.SizeBytes
	if need <= 0 {
		// Nothing meaningful to check against - don't invent a failure
		// for data Coldarr doesn't have.
		return nil
	}

	u, err := m.stat(entry.ToPath)
	if err != nil {
		// Can't observe real free space - fail safe rather than writing
		// blind on the plan's stale assumption alone.
		return fmt.Errorf("checking free space at %s before moving %q: %w", entry.ToPath, entry.Item.Title, err)
	}
	if uint64(need) > u.FreeBytes {
		return fmt.Errorf("refusing to move %q to %s: only %.1f GB free right now, need %.1f GB - plan may be stale, re-run Plan", entry.Item.Title, entry.ToPath, float64(u.FreeBytes)/(1<<30), float64(need)/(1<<30))
	}
	return nil
}

func (m *Movers) runOne(entry planner.MoveEntry, progress *Progress, idx int) {
	progress.setStatus(idx, StatusMoving, nil)

	if err := m.verifyRoom(entry); err != nil {
		progress.setStatus(idx, StatusFailed, err)
		return
	}

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
// Disk usage looking stable is a hint, never proof: a transfer can
// plateau for many seconds - a write-cache flush, the destination's own
// housekeeping, a network stall - while genuinely still in progress, and
// that plateau looks identical to a finished transfer from disk usage
// alone. Every path to "settled" below is therefore gated on
// confirmLanded actually agreeing with Radarr/Sonarr before returning;
// the disk-usage tracker only decides when it's worth paying for that
// check, not whether the move is done. A confirmLanded failure resets
// the stability count rather than looping immediately, so a stalled
// rescan doesn't get hammered every single interval.
//
// Disk-usage growth also can't tell a finished move that added no
// measurable usage - the item was already sitting at (or very near) the
// destination, or landed via a same-volume rename - from one that simply
// hasn't started yet, so that case is tracked separately (noGrowthChecks)
// rather than through the growth-based tracker, which never reports
// stability before any growth at all.
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
			if m.confirmLanded(entry) {
				return
			}
			tracker.stableCount = 0
			continue
		}

		if tracker.grown {
			noGrowthChecks = 0
			continue
		}
		noGrowthChecks++
		if noGrowthChecks >= stableTarget {
			if m.confirmLanded(entry) {
				return
			}
			noGrowthChecks = 0
		}
	}
}

// confirmLanded asks Radarr/Sonarr to rescan the item's folder - forcing
// it to recompute the item's location/size from the real filesystem
// rather than a possibly-stale cached value - and reports whether it now
// considers the item fully landed: its path under entry.ToPath, AND its
// freshly-rescanned size actually matching what was planned (see
// sizeLanded). Checking the path alone isn't enough: Radarr/Sonarr can
// update an item's recorded path before the underlying file transfer is
// completely finished, so a path-only check can say "landed" while the
// file is still short. This is the ground-truth check settle() gates
// every "looks done" signal on; see settle's doc comment for why disk
// usage alone isn't enough either.
func (m *Movers) confirmLanded(entry planner.MoveEntry) bool {
	switch entry.Item.ArrApp {
	case "radarr":
		if err := m.Radarr.RescanMovie(entry.Item.ID); err != nil {
			return false
		}
		size, path, found, err := m.Radarr.GetMovieSize(entry.Item.ID)
		return err == nil && found && isUnderPath(path, entry.ToPath) && sizeLanded(size, entry.Item.SizeBytes)
	case "sonarr":
		if err := m.Sonarr.RescanSeries(entry.Item.ID); err != nil {
			return false
		}
		size, path, found, err := m.Sonarr.GetSeriesSize(entry.Item.ID)
		return err == nil && found && isUnderPath(path, entry.ToPath) && sizeLanded(size, entry.Item.SizeBytes)
	default:
		return false
	}
}

// sizeLanded reports whether gotBytes - Radarr/Sonarr's freshly-rescanned,
// real read of the file on disk - is close enough to wantBytes (the
// item's expected size from the plan) to trust the transfer is actually
// finished, rather than a partial file that merely happens to already
// sit at the right path. A plain move never legitimately shrinks a file,
// so anything meaningfully short of wantBytes means it's still copying;
// a small tolerance absorbs harmless slack (e.g. a filesystem block-size
// rounding difference between how Coldarr and Radarr/Sonarr each compute
// "size on disk"), not genuine incompleteness.
func sizeLanded(gotBytes, wantBytes int64) bool {
	if wantBytes <= 0 {
		// No usable expected size to compare against - fall back to
		// trusting the rescanned path alone rather than blocking forever
		// on a check that can never be satisfied.
		return true
	}
	return gotBytes >= wantBytes*99/100
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
