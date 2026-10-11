// Package mover executes a move plan by asking the app that owns each item
// to relocate it - Radarr/Sonarr for library items, qBittorrent for
// downloads - so the owning app's database stays the source of truth.
// Coldarr never moves media files itself.
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
//
// That machinery - phases, per-volume queues, the room re-check before
// each move, and waiting for a move to land - is the same whichever app
// does the moving, and exists once, in Run. What differs is behind an
// Executor: how a move is requested, how the app confirms it landed, and
// what happens once it has. Movers is the Radarr/Sonarr one.
package mover

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/planner"
)

// Entry is one planned move, as the shared engine sees it - whichever app
// carries it out.
type Entry interface {
	// MoveTitle names the move in errors and progress.
	MoveTitle() string
	// MoveSize is how many bytes the move adds at its destination; zero or
	// less when unknown.
	MoveSize() int64
	// MoveDestination is the tier path the move writes to - what's
	// checked for room beforehand, and watched while it lands.
	MoveDestination() string
	// MovePhase orders dependent moves (see planner.MoveEntry.Phase).
	MovePhase() int
	// MoveCeiling is the destination's hard usage ceiling, in percent;
	// zero for none.
	MoveCeiling() float64
}

// Executor carries out one app's moves for the shared engine.
type Executor[E Entry] interface {
	// Move asks the app to start moving e. It returns once the app has
	// accepted the request - the move itself runs inside the app.
	Move(e E) error
	// Rejected reports whether err, returned by Move, proves the app
	// turned the request down, so nothing from it can still be writing.
	// Anything else - a request whose answer was lost, say - may have
	// been accepted, so the destination is treated as busy.
	Rejected(err error) bool
	// ConfirmLanded asks the app whether e has fully landed, and where.
	// An error wrapping ErrMoveFailed says the move has failed for good,
	// and waiting longer can't land it; any other error is retried.
	ConfirmLanded(e E) (landed bool, path string, err error)
	// Landed finishes a confirmed move - recording it, telling anything
	// downstream - and reports whether a downstream report succeeded
	// (see EntryProgress.Reported). An error is shown on the entry, but
	// the move still counts as landed: its capacity change has happened.
	Landed(e E, landedPath string) (reported bool, err error)
}

// ErrMoveFailed marks a ConfirmLanded error as final: the app reports the
// move itself went wrong, so there's nothing left to wait for.
var ErrMoveFailed = errors.New("move failed")

// MoveReporter is told where an item landed the moment its move is
// confirmed, so a downstream consumer of the library (in practice
// Jellyfin) can begin re-indexing it while the rest of the run is still
// moving. The mover depends on this narrow interface rather than on the
// jellyfin package directly: telling a consumer where media went is a
// courtesy to something downstream, not part of executing a plan, and
// nothing here should be able to fail a move.
type MoveReporter interface {
	// ReportMoved names the folder the item vacated and the folder it now
	// occupies, both as the owning Arr app reports them. An error is
	// advisory - the item is simply left for the caller to report again at
	// the end of the run (see EntryProgress.Reported).
	ReportMoved(oldPath, newPath string) error
}

// Options are the shared engine's settings.
type Options struct {
	// CheckStorage, when set, runs immediately before each move and fails
	// that move without contacting the app if it returns an error - a
	// drive can drop out partway through a run that takes hours.
	CheckStorage func() error

	// SettleCheckInterval, SettleStableChecks, and SettleMaxWait
	// control how long Run waits, after asking the app to move an item,
	// for it to actually land on disk before starting the next item
	// queued for the same volume. Zero values fall back to production
	// defaults; tests override them to run fast.
	SettleCheckInterval time.Duration
	SettleStableChecks  int
	SettleMaxWait       time.Duration

	// Serial runs every entry of a phase in one queue, one at a time,
	// whatever volume it's bound for.
	Serial bool

	// statFunc defaults to diskusage.Stat; tests override it to observe
	// and control settling without touching a real filesystem.
	statFunc func(path string) (diskusage.Usage, error)
}

func (o Options) stat(path string) (diskusage.Usage, error) {
	if o.statFunc != nil {
		return o.statFunc(path)
	}
	return diskusage.Stat(path)
}

// Movers runs library plans through Radarr/Sonarr.
type Movers struct {
	Radarr  *arrapi.RadarrClient
	Sonarr  *arrapi.SonarrClient
	History *history.Store

	// CheckStorage, when set, runs immediately before each move and fails
	// that move without contacting Radarr/Sonarr if it returns an error -
	// a drive can drop out partway through a run that takes hours.
	CheckStorage func() error

	// Reporter, when set, is handed each item's new location as soon as
	// that move is confirmed landed. Nil disables mid-run reporting
	// entirely, which is not a failure: every move still ends up reported
	// by whatever runs after the plan finishes.
	Reporter MoveReporter

	// SettleCheckInterval, SettleStableChecks, and SettleMaxWait
	// control how long Apply waits, after asking Radarr/Sonarr to move
	// an item, for it to actually land on disk before starting the next
	// item queued for the same volume. Zero values fall back to
	// production defaults; tests override them to run fast.
	SettleCheckInterval time.Duration
	SettleStableChecks  int
	SettleMaxWait       time.Duration

	// Logf defaults to log.Printf; tests override it to capture output.
	// Reporting is the only thing this package logs, because it's the only
	// thing it does whose outcome isn't already carried on Progress.
	Logf func(format string, args ...any)

	// statFunc defaults to diskusage.Stat; tests override it to observe
	// and control settling without touching a real filesystem.
	statFunc func(path string) (diskusage.Usage, error)
}

func (m *Movers) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (m *Movers) options() Options {
	return Options{
		CheckStorage:        m.CheckStorage,
		SettleCheckInterval: m.SettleCheckInterval,
		SettleStableChecks:  m.SettleStableChecks,
		SettleMaxWait:       m.SettleMaxWait,
		statFunc:            m.statFunc,
	}
}

type MoveStatus string

const (
	StatusPending MoveStatus = "pending"
	StatusMoving  MoveStatus = "moving"
	StatusDone    MoveStatus = "done"
	StatusFailed  MoveStatus = "failed"
)

// RunEntry is one entry of a run and how it's going.
type RunEntry[E Entry] struct {
	Entry  E
	Status MoveStatus
	Err    string
	// Timing includes the command, settling, and reporting, so estimates
	// reflect how long each move actually occupies its destination queue.
	Started  time.Time
	Finished time.Time
	volume   string
	// LandedPath is the item's own folder after the move, as the app
	// reports it once the transfer is confirmed landed - not the entry's
	// destination, which is only the tier root every item in that tier
	// shares. Consumers that need to point at the item itself (notifying
	// Jellyfin, which keys items by their exact path) need this; guessing
	// it by joining the destination with the old folder's name would be
	// wrong for any item Radarr/Sonarr renamed as part of the move. Empty
	// if the move never landed.
	LandedPath string
	// Reported records that a downstream consumer (see MoveReporter) was
	// successfully told about this move as it landed, rather than at the
	// end of the run.
	//
	// Consumers need this to avoid reporting the same path twice: Jellyfin
	// restarts its library-monitor debounce every time a path it already
	// has pending is reported again, so a redundant second report delays
	// the rescan the first one asked for. False also covers "the mid-run
	// report failed", which is why it means "still needs reporting" rather
	// than merely "no reporter was configured".
	Reported bool
}

// RunSnapshot is a point-in-time, race-free copy of a run's state, safe to
// read from any goroutine (e.g. a web handler polling for status while the
// run continues in the background).
type RunSnapshot[E Entry] struct {
	Started  time.Time
	Finished time.Time
	Done     bool
	Entries  []RunEntry[E]
}

func (s RunSnapshot[E]) Moved() []RunEntry[E] {
	return s.byStatus(StatusDone)
}

func (s RunSnapshot[E]) Failed() []RunEntry[E] {
	return s.byStatus(StatusFailed)
}

func (s RunSnapshot[E]) byStatus(status MoveStatus) []RunEntry[E] {
	var out []RunEntry[E]
	for _, e := range s.Entries {
		if e.Status == status {
			out = append(out, e)
		}
	}
	return out
}

// RunProgress is a live view of an in-progress (or completed) run.
type RunProgress[E Entry] struct {
	mu       sync.Mutex
	started  time.Time
	finished time.Time
	done     bool
	entries  []RunEntry[E]
	doneCh   chan struct{}
}

// The library's instantiation keeps the names everything outside this
// package has always used.
type (
	EntryProgress    = RunEntry[planner.MoveEntry]
	ProgressSnapshot = RunSnapshot[planner.MoveEntry]
	Progress         = RunProgress[planner.MoveEntry]
)

func newProgress(plan *planner.Plan, volumeOf map[string]uint64) *Progress {
	return newRunProgress(plan.Entries, volumeOf, false)
}

// serialQueue is the one queue every entry of a serial run shares.
const serialQueue = "serial"

func newRunProgress[E Entry](plan []E, volumeOf map[string]uint64, serial bool) *RunProgress[E] {
	entries := make([]RunEntry[E], len(plan))
	for i, e := range plan {
		volume := serialQueue
		if !serial {
			volume = volumeKey(e.MoveDestination(), volumeOf)
		}
		entries[i] = RunEntry[E]{Entry: e, Status: StatusPending, volume: volume}
	}
	return &RunProgress[E]{started: time.Now(), entries: entries, doneCh: make(chan struct{})}
}

func (p *RunProgress[E]) setStatus(i int, status MoveStatus, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[i].Status = status
	switch status {
	case StatusMoving:
		p.entries[i].Started = time.Now()
	case StatusDone, StatusFailed:
		p.entries[i].Finished = time.Now()
	}
	if err != nil {
		p.entries[i].Err = err.Error()
	}
}

func (p *RunProgress[E]) setLandedPath(i int, path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[i].LandedPath = path
}

func (p *RunProgress[E]) setReported(i int, reported bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[i].Reported = reported
}

func (p *RunProgress[E]) markDone() {
	p.mu.Lock()
	p.done = true
	p.finished = time.Now()
	p.mu.Unlock()
	close(p.doneCh)
}

// Wait blocks until the run has finished - every volume's queue drained.
// For callers (like the CLI) that want to run synchronously.
func (p *RunProgress[E]) Wait() {
	<-p.doneCh
}

// Snapshot returns a race-free copy of the current state.
func (p *RunProgress[E]) Snapshot() RunSnapshot[E] {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := make([]RunEntry[E], len(p.entries))
	copy(entries, p.entries)
	return RunSnapshot[E]{Started: p.started, Finished: p.finished, Done: p.done, Entries: entries}
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
// If any entry in a phase fails to land, later phases are not attempted:
// their capacity assumptions depend on every physical move in that phase.
// A failure after a confirmed landing (currently, persisting its history)
// remains visible in Progress but does not invalidate that capacity change.
// Entries blocked by an unmet prerequisite are marked failed explicitly so
// a completed Progress never leaves work silently pending.
func (m *Movers) Apply(plan *planner.Plan, volumeOf map[string]uint64) *Progress {
	return Run[planner.MoveEntry](arrExecutor{m}, plan.Entries, volumeOf, m.options())
}

// Run starts executing entries through exec and returns immediately with a
// *RunProgress the caller can poll or Wait() on - see Apply, which is Run
// for Radarr/Sonarr.
func Run[E Entry](exec Executor[E], entries []E, volumeOf map[string]uint64, opts Options) *RunProgress[E] {
	r := &runner[E]{exec: exec, opts: opts}
	progress := newRunProgress(entries, volumeOf, opts.Serial)

	byPhase := map[int][]int{}
	for i, e := range entries {
		byPhase[e.MovePhase()] = append(byPhase[e.MovePhase()], i)
	}
	phases := make([]int, 0, len(byPhase))
	for p := range byPhase {
		phases = append(phases, p)
	}
	sort.Ints(phases)

	go func() {
		for phaseIndex, phase := range phases {
			if r.runPhase(byPhase[phase], progress) {
				continue
			}

			for _, laterPhase := range phases[phaseIndex+1:] {
				for _, i := range byPhase[laterPhase] {
					progress.setStatus(i, StatusFailed, fmt.Errorf("not attempted: prerequisite phase %d failed", phase))
				}
			}
			break
		}
		progress.markDone()
	}()

	return progress
}

type runner[E Entry] struct {
	exec Executor[E]
	opts Options
}

// runPhase groups the given plan-entry indices by destination volume and
// runs each group's moves serially, with different volumes proceeding
// concurrently; it blocks until every move in this phase has finished
// (including settling), and reports whether all physical moves landed.
// This deliberately differs from every entry having StatusDone: a move can
// land successfully and then fail only while recording its history, which
// must remain visible without blocking phases that depend on the disk-space
// change that already happened.
//
// An accepted move whose landing cannot be confirmed, or a command whose
// response was lost after it may have been accepted, also stops the rest of
// that destination volume's queue. The transfer may still be running, so
// starting another write to the same volume would defeat the very
// serialization this package provides. Other volume groups in the same
// phase are independent and are allowed to finish.
func (r *runner[E]) runPhase(indices []int, progress *RunProgress[E]) bool {
	groups := map[string][]int{}
	var order []string
	for _, i := range indices {
		key := progress.entries[i].volume
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}

	var wg sync.WaitGroup
	var resultMu sync.Mutex
	phaseLanded := true
	markLandingUnmet := func() {
		resultMu.Lock()
		phaseLanded = false
		resultMu.Unlock()
	}
	for _, key := range order {
		groupIndices := groups[key]
		wg.Add(1)
		go func(groupIndices []int) {
			defer wg.Done()
			for groupIndex, i := range groupIndices {
				landed, destinationSafe := r.runOne(progress.entries[i].Entry, progress, i)
				if !landed {
					markLandingUnmet()
				}
				if destinationSafe {
					continue
				}

				for _, blocked := range groupIndices[groupIndex+1:] {
					progress.setStatus(blocked, StatusFailed, fmt.Errorf("not attempted: a previous move command to this destination volume may still be running"))
					markLandingUnmet()
				}
				break
			}
		}(groupIndices)
	}
	wg.Wait()

	resultMu.Lock()
	defer resultMu.Unlock()
	return phaseLanded
}

func volumeKey(path string, volumeOf map[string]uint64) string {
	if dev, ok := volumeOf[path]; ok {
		return fmt.Sprintf("dev:%d", dev)
	}
	return "path:" + path
}

// verifyRoom re-stats the destination immediately before firing the move
// and refuses to proceed if it doesn't actually have room for the item
// right now. The plan's own capacity math (see internal/planner) is
// computed once, from a snapshot taken before the run started - a
// long-running apply, or anything else writing to the same destination in
// the meantime (another download landing, a concurrent process, drift from
// an earlier bug), can leave real free space short of what the plan
// assumed by the time this specific entry's turn comes up. This is the
// last line of defense against ever handing an app a move with nowhere
// for it to actually land or beyond the destination tier's hard usage
// ceiling - independent of whether the plan (or anything upstream of it)
// got the arithmetic right.
func verifyRoom[E Entry](stat func(string) (diskusage.Usage, error), entry E) error {
	need := entry.MoveSize()
	if need <= 0 {
		// Nothing meaningful to check against - don't invent a failure
		// for data Coldarr doesn't have.
		return nil
	}

	u, err := stat(entry.MoveDestination())
	if err != nil {
		// Can't observe real free space - fail safe rather than writing
		// blind on the plan's stale assumption alone.
		return fmt.Errorf("checking free space at %s before moving %q: %w", entry.MoveDestination(), entry.MoveTitle(), err)
	}
	needBytes := uint64(need)
	if needBytes > u.FreeBytes {
		return fmt.Errorf("refusing to move %q to %s: only %.1f GB free right now, need %.1f GB - plan may be stale, re-run Plan", entry.MoveTitle(), entry.MoveDestination(), float64(u.FreeBytes)/(1<<30), float64(need)/(1<<30))
	}

	if ceiling := entry.MoveCeiling(); ceiling > 0 {
		projectedPercent := diskusage.PercentUsed(u.UsedBytes+needBytes, u.FreeBytes-needBytes)
		if projectedPercent > ceiling {
			return fmt.Errorf("refusing to move %q to %s: it would leave the destination %.1f%% used, above its %.1f%% hard ceiling - plan may be stale, re-run Plan", entry.MoveTitle(), entry.MoveDestination(), projectedPercent, ceiling)
		}
	}
	return nil
}

func (m *Movers) verifyRoom(entry planner.MoveEntry) error {
	return verifyRoom(m.options().stat, entry)
}

// runOne reports both whether the physical move landed and whether it is
// safe to start another move to the same destination volume. A failure
// after confirmed landing (currently history persistence) returns landed
// even though its visible status is Failed. Failures before the app
// accepted a move leave the volume safe but do not satisfy phase
// prerequisites. A settle failure does neither: the accepted transfer may
// still be writing.
func (r *runner[E]) runOne(entry E, progress *RunProgress[E], idx int) (landed, destinationSafe bool) {
	progress.setStatus(idx, StatusMoving, nil)

	if r.opts.CheckStorage != nil {
		if err := r.opts.CheckStorage(); err != nil {
			progress.setStatus(idx, StatusFailed, err)
			return false, true
		}
	}

	if err := verifyRoom(r.opts.stat, entry); err != nil {
		progress.setStatus(idx, StatusFailed, err)
		return false, true
	}

	if err := r.exec.Move(entry); err != nil {
		progress.setStatus(idx, StatusFailed, err)
		// A definite refusal means no transfer from this entry is
		// running, so the volume itself is safe for independent
		// same-phase work. Anything ambiguous - the app may have accepted
		// the move before the answer was lost - conservatively stops
		// this destination queue until a later apply can establish fresh
		// state.
		return false, r.exec.Rejected(err)
	}

	landedPath, err := r.settle(entry)
	if err != nil {
		progress.setStatus(idx, StatusFailed, err)
		return false, false
	}
	progress.setLandedPath(idx, landedPath)

	reported, err := r.exec.Landed(entry, landedPath)
	progress.setReported(idx, reported)
	if err != nil {
		progress.setStatus(idx, StatusFailed, err)
		return true, true
	}

	progress.setStatus(idx, StatusDone, nil)
	return true, true
}

// settle waits for entry's move to actually land on the destination's
// filesystem before returning, so the next item queued for this same
// volume doesn't start writing concurrently with it. The apps' move APIs
// return once the operation is queued, not once the bytes are actually on
// disk, so this watches disk usage directly rather than trusting the API
// response alone - true regardless of exactly how the app implements the
// move internally.
//
// Disk usage looking stable is a hint, never proof: a transfer can
// plateau for many seconds - a write-cache flush, the destination's own
// housekeeping, a network stall - while genuinely still in progress, and
// that plateau looks identical to a finished transfer from disk usage
// alone. Every path to "settled" below is therefore gated on
// ConfirmLanded actually agreeing with the app before returning; the
// disk-usage tracker only decides when it's worth paying for that check,
// not whether the move is done. A ConfirmLanded failure resets the
// stability count rather than looping immediately, so a stalled rescan
// doesn't get hammered every single interval - unless it says the move
// has failed for good (ErrMoveFailed), which ends the wait at once.
//
// Disk-usage growth also can't tell a finished move that added no
// measurable usage - the item was already sitting at (or very near) the
// destination, or landed via a same-volume rename - from one that simply
// hasn't started yet, so that case is tracked separately (noGrowthChecks)
// rather than through the growth-based tracker, which never reports
// stability before any growth at all.
// It returns the item's own post-move path alongside its verdict
// (see RunEntry.LandedPath).
func (r *runner[E]) settle(entry E) (string, error) {
	interval := r.opts.SettleCheckInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	stableTarget := r.opts.SettleStableChecks
	if stableTarget <= 0 {
		stableTarget = 3
	}
	maxWait := r.opts.SettleMaxWait
	if maxWait <= 0 {
		maxWait = 6 * time.Hour
	}

	baseline, err := r.opts.stat(entry.MoveDestination())
	if err != nil {
		return "", fmt.Errorf("move command was accepted, but checking destination %s while waiting for %q to land failed: %w", entry.MoveDestination(), entry.MoveTitle(), err)
	}

	tracker := newSettleTracker(baseline.UsedBytes, entry.MoveSize())
	deadline := time.Now().Add(maxWait)
	noGrowthChecks := 0
	var lastConfirmationErr error

	confirm := func() (bool, string, error) {
		landed, path, err := r.exec.ConfirmLanded(entry)
		if err != nil && errors.Is(err, ErrMoveFailed) {
			return false, "", err
		}
		lastConfirmationErr = err
		return landed, path, nil
	}

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		u, err := r.opts.stat(entry.MoveDestination())
		if err != nil {
			return "", fmt.Errorf("move command was accepted, but checking destination %s while waiting for %q to land failed: %w", entry.MoveDestination(), entry.MoveTitle(), err)
		}

		if tracker.observe(u.UsedBytes, stableTarget) {
			landed, path, err := confirm()
			if err != nil {
				return "", err
			}
			if landed {
				return path, nil
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
			landed, path, err := confirm()
			if err != nil {
				return "", err
			}
			if landed {
				return path, nil
			}
			noGrowthChecks = 0
		}
	}

	err = fmt.Errorf("move command was accepted, but landing of %q at %s could not be confirmed within %s", entry.MoveTitle(), entry.MoveDestination(), maxWait)
	if lastConfirmationErr != nil {
		return "", fmt.Errorf("%w: last confirmation failed: %w", err, lastConfirmationErr)
	}
	return "", err
}

// settleTracker decides whether a transfer looks complete from a series
// of disk-usage-used-bytes readings alone, with no knowledge of the app's
// internal state. It requires usage to have grown by roughly the item's
// expected size before it will ever consider the reading "stable" -
// otherwise a transfer that simply hasn't started yet (usage unchanged
// because nothing has happened) would look identical to one that's
// already finished.
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
