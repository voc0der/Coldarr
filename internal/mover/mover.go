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
	"errors"
	"fmt"
	"log"
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

type Movers struct {
	Radarr  *arrapi.RadarrClient
	Sonarr  *arrapi.SonarrClient
	History *history.Store

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
	// LandedPath is the item's own folder after the move, as Radarr/Sonarr
	// reports it once the transfer is confirmed landed - not Entry.ToPath,
	// which is only the destination tier root every item in that tier
	// shares. Consumers that need to point at the item itself (notifying
	// Jellyfin, which keys items by their exact path) need this; guessing
	// it by joining ToPath with the old folder's name would be wrong for
	// any item Radarr/Sonarr renamed as part of the move. Empty if the
	// move never landed.
	LandedPath string
	// Reported records that Movers.Reporter was successfully told about
	// this move as it landed, rather than at the end of the run.
	//
	// Consumers need this to avoid reporting the same path twice: Jellyfin
	// restarts its library-monitor debounce every time a path it already
	// has pending is reported again, so a redundant second report delays
	// the rescan the first one asked for. False also covers "the mid-run
	// report failed", which is why it means "still needs reporting" rather
	// than merely "no reporter was configured".
	Reported bool
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

func (p *Progress) setLandedPath(i int, path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[i].LandedPath = path
}

func (p *Progress) setReported(i int, reported bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries[i].Reported = reported
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
// If any entry in a phase fails to land, later phases are not attempted:
// their capacity assumptions depend on every physical move in that phase.
// A failure after a confirmed landing (currently, persisting its history)
// remains visible in Progress but does not invalidate that capacity change.
// Entries blocked by an unmet prerequisite are marked failed explicitly so
// a completed Progress never leaves work silently pending.
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
		for phaseIndex, phase := range phases {
			if m.runPhase(plan, byPhase[phase], progress, volumeOf) {
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
func (m *Movers) runPhase(plan *planner.Plan, indices []int, progress *Progress, volumeOf map[string]uint64) bool {
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
				landed, destinationSafe := m.runOne(plan.Entries[i], progress, i)
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

// verifyRoom re-stats entry.ToPath immediately before firing the move and
// refuses to proceed if it doesn't actually have room for the item right
// now. The plan's own capacity math (see internal/planner) is computed
// once, from a snapshot taken before the run started - a long-running
// apply, or anything else writing to the same destination in the
// meantime (another download landing, a concurrent process, drift from
// an earlier bug), can leave real free space short of what the plan
// assumed by the time this specific entry's turn comes up. This is the
// last line of defense against ever handing Radarr/Sonarr a move with
// nowhere for it to actually land or beyond the destination tier's hard
// usage ceiling - independent of whether the plan (or anything upstream
// of it) got the arithmetic right.
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
	needBytes := uint64(need)
	if needBytes > u.FreeBytes {
		return fmt.Errorf("refusing to move %q to %s: only %.1f GB free right now, need %.1f GB - plan may be stale, re-run Plan", entry.Item.Title, entry.ToPath, float64(u.FreeBytes)/(1<<30), float64(need)/(1<<30))
	}

	if entry.MaxUsedPercent > 0 {
		projectedPercent := diskusage.PercentUsed(u.UsedBytes+needBytes, u.FreeBytes-needBytes)
		if projectedPercent > entry.MaxUsedPercent {
			return fmt.Errorf("refusing to move %q to %s: it would leave the destination %.1f%% used, above its %.1f%% hard ceiling - plan may be stale, re-run Plan", entry.Item.Title, entry.ToPath, projectedPercent, entry.MaxUsedPercent)
		}
	}
	return nil
}

// runOne reports both whether the physical move landed and whether it is
// safe to start another move to the same destination volume. A failure
// after confirmed landing (currently history persistence) returns landed
// even though its visible status is Failed. Failures before Arr accepted a
// command leave the volume safe but do not satisfy phase prerequisites. A
// settle failure does neither: the accepted transfer may still be writing.
func (m *Movers) runOne(entry planner.MoveEntry, progress *Progress, idx int) (landed, destinationSafe bool) {
	progress.setStatus(idx, StatusMoving, nil)

	if err := m.verifyRoom(entry); err != nil {
		progress.setStatus(idx, StatusFailed, err)
		return false, true
	}

	var err error
	switch entry.Item.ArrApp {
	case "radarr":
		err = m.Radarr.MoveMovies([]int{entry.Item.ID}, entry.ToPath)
	case "sonarr":
		err = m.Sonarr.MoveSeries([]int{entry.Item.ID}, entry.ToPath)
	default:
		progress.setStatus(idx, StatusFailed, fmt.Errorf("unknown arr app %q", entry.Item.ArrApp))
		return false, true
	}
	if err != nil {
		progress.setStatus(idx, StatusFailed, err)
		// A concrete non-2xx response means Arr rejected the command, so
		// no transfer from this entry is running and the volume itself is
		// safe for independent same-phase work. Transport and response-
		// decoding failures are ambiguous: Arr may have accepted the move
		// before the response was lost, so conservatively stop this
		// destination queue until a later apply can establish fresh state.
		var statusErr *arrapi.StatusError
		return false, errors.As(err, &statusErr)
	}

	landedPath, err := m.settle(entry)
	if err != nil {
		progress.setStatus(idx, StatusFailed, err)
		return false, false
	}
	progress.setLandedPath(idx, landedPath)
	progress.setReported(idx, m.reportMoved(entry, landedPath))

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
		return true, true
	}

	progress.setStatus(idx, StatusDone, nil)
	return true, true
}

// reportMoved hands this item's old and new folders to Reporter inline
// with the move that just landed, rather than leaving every item in the
// run to be reported in one batch at the end, and reports whether that
// succeeded.
//
// Timing is the whole point. A consumer's indexing work is slow and
// starts late - Jellyfin sits on a reported path for a minute before
// touching it, then re-validates the entire containing library root - so
// a batch delivered after the last move means all of that begins from a
// standstill, with nothing left to overlap it. Delivered here it runs
// against the remaining moves, which take minutes to hours apiece.
//
// Synchronous, not fired into a goroutine: this is one small POST to a
// service the run is going to poll anyway, bounded by that client's own
// timeout, which is nothing beside the transfer it follows. Waiting for it
// is also what makes the result trustworthy enough to record, so the
// end-of-run pass can pick up precisely the items still needing a report
// instead of re-reporting everything and resetting their debounce.
//
// Never fatal, and deliberately not reflected in the entry's status: a
// consumer being unreachable says nothing about whether the media moved,
// which Radarr/Sonarr already confirmed.
func (m *Movers) reportMoved(entry planner.MoveEntry, landedPath string) bool {
	if m.Reporter == nil {
		return false
	}
	if err := m.Reporter.ReportMoved(entry.Item.Path, landedPath); err != nil {
		m.logf("mover: reporting %q moved to %s failed, leaving it for the end of the run: %v", entry.Item.Title, landedPath, err)
		return false
	}
	return true
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
// It returns the item's own post-move folder path alongside its verdict
// (see EntryProgress.LandedPath).
func (m *Movers) settle(entry planner.MoveEntry) (string, error) {
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
		return "", fmt.Errorf("move command was accepted, but checking destination %s while waiting for %q to land failed: %w", entry.ToPath, entry.Item.Title, err)
	}

	tracker := newSettleTracker(baseline.UsedBytes, entry.Item.SizeBytes)
	deadline := time.Now().Add(maxWait)
	noGrowthChecks := 0
	var lastConfirmationErr error

	for time.Now().Before(deadline) {
		time.Sleep(interval)

		u, err := m.stat(entry.ToPath)
		if err != nil {
			return "", fmt.Errorf("move command was accepted, but checking destination %s while waiting for %q to land failed: %w", entry.ToPath, entry.Item.Title, err)
		}

		if tracker.observe(u.UsedBytes, stableTarget) {
			landed, path, err := m.confirmLanded(entry)
			if landed {
				return path, nil
			}
			lastConfirmationErr = err
			tracker.stableCount = 0
			continue
		}

		if tracker.grown {
			noGrowthChecks = 0
			continue
		}
		noGrowthChecks++
		if noGrowthChecks >= stableTarget {
			landed, path, err := m.confirmLanded(entry)
			if landed {
				return path, nil
			}
			lastConfirmationErr = err
			noGrowthChecks = 0
		}
	}

	err = fmt.Errorf("move command was accepted, but landing of %q at %s could not be confirmed within %s", entry.Item.Title, entry.ToPath, maxWait)
	if lastConfirmationErr != nil {
		return "", fmt.Errorf("%w: last confirmation failed: %w", err, lastConfirmationErr)
	}
	return "", err
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
// It also returns the item's own folder path as Radarr/Sonarr now reports
// it, which is the only authoritative source for where the item actually
// ended up - see EntryProgress.LandedPath.
func (m *Movers) confirmLanded(entry planner.MoveEntry) (bool, string, error) {
	switch entry.Item.ArrApp {
	case "radarr":
		if err := m.Radarr.RescanMovie(entry.Item.ID); err != nil {
			return false, "", fmt.Errorf("rescanning movie: %w", err)
		}
		size, path, found, err := m.Radarr.GetMovieSize(entry.Item.ID)
		return landingMatches("Radarr", entry, size, path, found, err)
	case "sonarr":
		if err := m.Sonarr.RescanSeries(entry.Item.ID); err != nil {
			return false, "", fmt.Errorf("rescanning series: %w", err)
		}
		size, path, found, err := m.Sonarr.GetSeriesSize(entry.Item.ID)
		return landingMatches("Sonarr", entry, size, path, found, err)
	default:
		return false, "", fmt.Errorf("unknown arr app %q", entry.Item.ArrApp)
	}
}

func landingMatches(app string, entry planner.MoveEntry, size int64, path string, found bool, err error) (bool, string, error) {
	if err != nil {
		return false, "", fmt.Errorf("reading item from %s after rescan: %w", app, err)
	}
	if !found {
		return false, "", fmt.Errorf("%s no longer reports the item", app)
	}
	if !isUnderPath(path, entry.ToPath) {
		return false, "", fmt.Errorf("%s reports path %q, expected it under %q", app, path, entry.ToPath)
	}
	if !sizeLanded(size, entry.Item.SizeBytes) {
		return false, "", fmt.Errorf("%s reports %d bytes, expected approximately %d", app, size, entry.Item.SizeBytes)
	}
	return true, path, nil
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
