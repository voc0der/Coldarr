package webui

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/engine"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/notify"
	"github.com/vocoder/coldarr/internal/torrents"
)

// downloadsRun is a downloads apply in flight - applyRun's counterpart.
// It holds the same mover lock, so only one apply of either kind ever
// runs at a time.
type downloadsRun struct {
	progress *mover.RunProgress[torrents.MoveEntry]
	lock     *mover.Lock
	// active stays set until the whole run is over: moves, then relabels,
	// then releasing the lock. done is closed at the same moment.
	active atomic.Bool
	done   chan struct{}

	mu        sync.Mutex
	relabeled int
	relabels  int
	relabelEr []string
}

func (r *downloadsRun) note() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.relabels == 0 {
		return ""
	}
	if r.active.Load() && r.relabeled+len(r.relabelEr) == 0 {
		return fmt.Sprintf("Once the moves are done, %d download(s) already on seed storage get the seed label.", r.relabels)
	}
	note := fmt.Sprintf("Relabeled %d of %d download(s) already on seed storage.", r.relabeled, r.relabels)
	if len(r.relabelEr) > 0 {
		note += " Failed: " + strings.Join(r.relabelEr, "; ")
	}
	return note
}

type downloadMoveView struct {
	Name       string
	CrossSeeds int
	Size       string
	Finished   string
	From       string
	To         string
}

type relabelView struct {
	Name     string
	Category string
}

type downloadHeldView struct {
	Name   string
	State  string
	Reason string
}

type downloadEventView struct {
	At     string
	Action string
	Names  string
	Detail string
	Size   string
}

type downloadsPlanView struct {
	Error     string
	Empty     bool
	Moves     []downloadMoveView
	Relabels  []relabelView
	TotalSize string
	Usage     []usageRowView
	Warnings  []string
	// Counts, Held and Waiting describe everything else: what's not moving
	// this time, and why.
	Counts      downloadCounts
	Held        []downloadHeldView
	NextSettled string

	// Done-seeding: what removal would do, by mode.
	RemovalMode  string
	DoneSeeding  int
	Removals     []removalView
	Deferred     int
	RemovalHeld  []downloadHeldView
	RemovalError string
}

type removalView struct {
	Name       string
	CrossSeeds int
	Size       string
	Seeded     string
	Rule       string
	Where      string
	KeptFor    string
}

type downloadsPageData struct {
	Title  string
	Run    applyStatusData
	Plan   downloadsPlanView
	Recent []downloadEventView
	// Removed and RemoveError report a Remove now click.
	Removed     string
	RemoveError string
}

// recentDownloadEvents is how much of the download history the Downloads
// page shows.
const recentDownloadEvents = 20

func (s *Server) buildDownloadsPageData() downloadsPageData {
	data := downloadsPageData{Title: "Downloads", Run: s.currentDownloadsApplyStatus()}
	eng, err := s.newEngine()
	if err != nil {
		data.Plan.Error = err.Error()
		return data
	}
	for _, ev := range eng.DownloadLog.Recent(recentDownloadEvents) {
		data.Recent = append(data.Recent, downloadEventViewOf(ev))
	}
	if !data.Run.Running {
		data.Plan = s.buildDownloadsPlanView(eng)
	}
	return data
}

func downloadEventViewOf(ev history.DownloadEvent) downloadEventView {
	view := downloadEventView{At: ev.At.Format("2006-01-02 15:04"), Action: ev.Action, Names: strings.Join(uniqueNames(ev.Names), ", "), Detail: ev.Detail}
	if ev.SizeBytes > 0 {
		view.Size = fmt.Sprintf("%.1f GB", float64(ev.SizeBytes)/(1<<30))
	}
	if view.Detail == "" && ev.ToPath != "" {
		view.Detail = "to " + ev.ToPath
	}
	return view
}

func uniqueNames(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// buildDownloadsPlanView is the Downloads page's dry run - refused
// outright, like the Plan page's, while anything is still moving or a
// drive is missing, since a plan built then would be built on bad numbers.
func (s *Server) buildDownloadsPlanView(eng *engine.Engine) downloadsPlanView {
	var view downloadsPlanView
	if !eng.DownloadsEnabled() {
		view.Error = "Download tiering needs qBittorrent connected and at least one downloads tier - set them up under Settings > Connections and Settings > Storage tiers."
		return view
	}
	if busy, detail, err := eng.MovesInFlight(); err != nil {
		view.Error = err.Error()
		return view
	} else if busy {
		view.Error = detail
		return view
	}
	if err := eng.CheckStorage(); err != nil {
		view.Error = err.Error()
		return view
	}

	now := time.Now()
	inv, err := eng.BuildDownloadsInventory(now, true)
	if err != nil {
		view.Error = err.Error()
		return view
	}
	plan := eng.BuildDownloadsPlan(inv)

	view.Empty = len(plan.Entries) == 0 && len(plan.Relabels) == 0
	view.Warnings = append(append([]string{}, inv.Warnings...), plan.Warnings...)
	var total int64
	for _, e := range plan.Entries {
		total += e.SizeBytes
		view.Moves = append(view.Moves, downloadMoveView{
			Name:       e.Name,
			CrossSeeds: len(e.Hashes) - 1,
			Size:       fmt.Sprintf("%.1f GB", float64(e.SizeBytes)/(1<<30)),
			Finished:   ago(now, e.CompletedAt),
			From:       e.ContentPath,
			To:         e.NewContentPath,
		})
	}
	view.TotalSize = fmt.Sprintf("%.1f GB", float64(total)/(1<<30))
	for _, r := range plan.Relabels {
		view.Relabels = append(view.Relabels, relabelView{Name: r.Name, Category: r.Category})
	}

	counts := inv.Count()
	view.Counts = downloadCounts{Active: counts[torrents.Active], Waiting: counts[torrents.Waiting], Ready: counts[torrents.Ready], Held: counts[torrents.Held], Parked: counts[torrents.Parked]}
	var next time.Time
	for _, d := range inv.Downloads {
		switch d.State {
		case torrents.Held:
			view.Held = append(view.Held, downloadHeldView{Name: d.Name, State: "held", Reason: d.Reason})
		case torrents.Waiting:
			if next.IsZero() || d.SettledAt.Before(next) {
				next = d.SettledAt
			}
		case torrents.Parked:
			if d.Reason != "" {
				view.Held = append(view.Held, downloadHeldView{Name: d.Name, State: "on seed storage", Reason: d.Reason})
			}
		}
	}
	if !next.IsZero() {
		view.NextSettled = next.Format("2006-01-02 15:04")
	}

	view.RemovalMode = string(eng.Cfg.Torrents.Removal.EffectiveMode())
	for _, d := range inv.Downloads {
		if d.TierPath != "" && d.State != torrents.Ignored && d.State != torrents.Active && d.Seeding.Met {
			view.DoneSeeding++
		}
	}
	if removals, err := eng.PlanRemovals(inv); err != nil {
		view.RemovalError = err.Error()
	} else {
		for _, r := range removals.Removals {
			view.Removals = append(view.Removals, removalViewOf(r))
		}
		for _, r := range removals.Deferred {
			view.Deferred += len(r.Hashes)
		}
		for _, h := range removals.Held {
			view.RemovalHeld = append(view.RemovalHeld, downloadHeldView{Name: h.Name, Reason: h.Reason})
		}
	}

	before := inv.UsableUsage()
	for _, tier := range inv.Tiers {
		for _, path := range tier.Paths {
			b, ok := before[path]
			a, okAfter := plan.FinalUsage[filepath.Clean(path)]
			if !ok || !okAfter || fmt.Sprintf("%.1f", b.UsedPercent) == fmt.Sprintf("%.1f", a.UsedPercent) {
				continue
			}
			view.Usage = append(view.Usage, usageRowView{Tier: tier.Name, Path: path, Before: b.UsedPercent, After: a.UsedPercent})
		}
	}
	return view
}

// ago says how long before now t was, in days or hours.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
	return fmt.Sprintf("%d hours ago", int(d.Hours()))
}

func (s *Server) handleDownloadsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "downloads", s.buildDownloadsPageData())
}

func (s *Server) renderDownloadsError(w http.ResponseWriter, err error) {
	data := s.buildDownloadsPageData()
	data.Plan = downloadsPlanView{Error: err.Error()}
	s.render(w, "downloads", data)
}

// handleDownloadsApplyStart builds a fresh downloads plan and starts it in
// the background - one move at a time - then redirects back to the page,
// which follows the run live.
func (s *Server) handleDownloadsApplyStart(w http.ResponseWriter, r *http.Request) {
	if s.applyInFlight() {
		http.Redirect(w, r, "/downloads", http.StatusSeeOther)
		return
	}
	eng, err := s.newEngine()
	if err != nil {
		s.renderDownloadsError(w, err)
		return
	}
	if !eng.DownloadsEnabled() {
		s.renderDownloadsError(w, fmt.Errorf("download tiering needs qBittorrent connected and at least one downloads tier"))
		return
	}
	inv, err := eng.BuildDownloadsInventory(time.Now(), true)
	if err != nil {
		s.renderDownloadsError(w, err)
		return
	}
	plan := eng.BuildDownloadsPlan(inv)
	if len(plan.Entries) == 0 && len(plan.Relabels) == 0 {
		http.Redirect(w, r, "/downloads", http.StatusSeeOther)
		return
	}
	if _, err := s.startDownloadsApply(eng, inv, plan); err != nil {
		s.renderDownloadsError(w, err)
		return
	}
	http.Redirect(w, r, "/downloads", http.StatusSeeOther)
}

// startDownloadsApply is startApply for downloads: the same gates and the
// same mover lock, then the plan's moves - strictly one at a time, each
// confirmed finished and labeled before the next begins - followed by any
// relabels of downloads already on seed storage. Shared by the Apply
// button and the scheduled "Move Settled Downloads" task.
func (s *Server) startDownloadsApply(eng *engine.Engine, inv *engine.DownloadsInventory, plan *torrents.Plan) (*downloadsRun, error) {
	// As in startApply: moves already handed to Radarr/Sonarr or
	// qBittorrent carry on across Coldarr restarts, and only they can
	// say so. A failed check refuses rather than assuming idle.
	if busy, detail, err := eng.MovesInFlight(); err != nil {
		return nil, err
	} else if busy {
		return nil, fmt.Errorf("refusing to apply: %s", detail)
	}
	if err := eng.CheckStorage(); err != nil {
		return nil, err
	}

	lock, err := mover.AcquireLock(filepath.Dir(s.cfgPath))
	if err != nil {
		return nil, err
	}

	exec := eng.DownloadsExecutor()
	run := &downloadsRun{lock: lock, relabels: len(plan.Relabels), done: make(chan struct{})}
	run.progress = mover.Run[torrents.MoveEntry](exec, plan.Entries, inv.VolumeOf(), eng.DownloadsOptions())
	run.active.Store(true)

	s.applyMu.Lock()
	s.currentDownloadsRun = run
	s.applyMu.Unlock()

	go func() {
		// Released before the run stops counting as active, never after -
		// see startApply.
		defer close(run.done)
		defer run.active.Store(false)
		defer func() { _ = lock.Release() }()
		run.progress.Wait()

		for _, r := range plan.Relabels {
			err := exec.Relabel(r)
			run.mu.Lock()
			if err != nil {
				run.relabelEr = append(run.relabelEr, err.Error())
				log.Printf("webui: %v", err)
			} else {
				run.relabeled++
			}
			run.mu.Unlock()
		}
	}()
	return run, nil
}

func (s *Server) currentDownloadsApplyStatus() applyStatusData {
	s.applyMu.Lock()
	run := s.currentDownloadsRun
	s.applyMu.Unlock()
	if run == nil {
		return applyStatusData{NoRun: true}
	}

	snap := run.progress.Snapshot()
	if snap.Done && !run.active.Load() && time.Since(snap.Finished) > finishedResultTTL {
		return applyStatusData{NoRun: true}
	}
	data := applyStatusData{
		PollURL:    "/downloads/apply/status/partial",
		OneAtATime: true,
		Note:       run.note(),
		Running:    run.active.Load(),
		Progress:   buildApplyProgress(snap, time.Now()),
	}
	if snap.Done {
		data.Finished = snap.Finished.Format("2006-01-02 15:04")
	}
	for _, e := range snap.Entries {
		data.Entries = append(data.Entries, applyStatusEntryView{
			Title:  e.Entry.Name,
			To:     fmt.Sprintf("%s (%s)", e.Entry.ToTier, e.Entry.Location),
			Status: string(e.Status),
			Err:    e.Err,
		})
		switch e.Status {
		case mover.StatusDone:
			data.MovedCount++
		case mover.StatusFailed:
			data.FailedCount++
		}
	}
	return data
}

// handleDownloadsApplyStatusPartial is the polled status fragment - see
// handleApplyStatusPartial.
func (s *Server) handleDownloadsApplyStatusPartial(w http.ResponseWriter, r *http.Request) {
	data := s.currentDownloadsApplyStatus()
	if !data.Running {
		w.Header().Set("HX-Refresh", "true")
	}
	s.renderPartial(w, "downloads", "apply_status_table", data)
}

// runScheduledDownloadsPlan is the scheduled counterpart to the Downloads
// page's Move button - runScheduledPlan for downloads, with the same rules
// for when a tick is skipped (and retried next tick) versus counted as the
// run and notified.
func (s *Server) runScheduledDownloadsPlan(now time.Time) {
	if s.applyInFlight() {
		log.Printf("scheduler: run-torrent-plan is due, but an apply is already in flight - skipping this tick, will retry next tick")
		return
	}

	n := s.notifier()
	fail := func(title string, err error) {
		log.Printf("scheduler: run-torrent-plan: %v", err)
		n.Summary(title, err.Error(), notify.LevelFailure)
		s.recordTaskRan(taskRunTorrentPlan, now)
	}

	eng, err := s.newEngine()
	if err != nil {
		fail("Scheduled downloads move failed", err)
		return
	}
	if !eng.DownloadsEnabled() {
		fail("Scheduled downloads move failed", fmt.Errorf("download tiering needs qBittorrent connected and at least one downloads tier"))
		return
	}
	// Moves still running inside Radarr/Sonarr or qBittorrent drain on
	// their own: skip without counting the run, so the next tick retries.
	if busy, detail, err := eng.MovesInFlight(); err != nil {
		log.Printf("scheduler: run-torrent-plan: %v - skipping this tick", err)
		return
	} else if busy {
		log.Printf("scheduler: run-torrent-plan is due, but %s - skipping this tick, will retry next tick", detail)
		return
	}
	if err := eng.CheckStorage(); err != nil {
		fail("Scheduled downloads move refused", err)
		return
	}

	inv, err := eng.BuildDownloadsInventory(now, true)
	if err != nil {
		fail("Scheduled downloads move failed", err)
		return
	}
	plan := eng.BuildDownloadsPlan(inv)
	warnings := append(append([]string{}, inv.Warnings...), plan.Warnings...)
	if len(plan.Entries) == 0 && len(plan.Relabels) == 0 {
		s.recordTaskRan(taskRunTorrentPlan, now)
		level := notify.LevelInfo
		body := "No settled download is clear to move, with room on seed storage."
		if len(warnings) > 0 {
			level = notify.LevelWarning
			body += " " + n.JoinLines(warnings)
		}
		n.Summary("Scheduled downloads move: nothing to move", body, level)
		return
	}

	run, err := s.startDownloadsApply(eng, inv, plan)
	if err != nil {
		// Most likely the mover lock is held - a manual apply, or the
		// CLI - so this is a skip, retried next tick.
		log.Printf("scheduler: run-torrent-plan: %v", err)
		return
	}
	s.recordTaskRan(taskRunTorrentPlan, now)

	go func() {
		<-run.done
		snap := run.progress.Snapshot()
		moved, failed := snap.Moved(), snap.Failed()
		level := notify.LevelSuccess
		if len(failed) > 0 || len(warnings) > 0 {
			level = notify.LevelWarning
		}
		body := fmt.Sprintf("moved %s, failed %s", n.Bold(strconv.Itoa(len(moved))), n.Bold(strconv.Itoa(len(failed))))
		if note := run.note(); note != "" {
			body += " - " + note
		}
		if len(warnings) > 0 {
			body += fmt.Sprintf(" - %d warning(s), see the Downloads page", len(warnings))
		}
		n.Summary("Scheduled downloads move finished", body, level)
		for _, e := range moved {
			n.Item("Moved "+e.Entry.Name, fmt.Sprintf("to %s (%s)", n.Bold(e.Entry.ToTier), n.Code(e.Entry.Location)), notify.LevelSuccess)
		}
		for _, e := range failed {
			n.Item("Failed to move "+e.Entry.Name, n.Code(e.Err), notify.LevelFailure)
		}
		for _, w := range warnings {
			n.Item("Downloads warning", w, notify.LevelWarning)
		}
	}()
}

func removalViewOf(r torrents.Removal) removalView {
	return removalView{
		Name:       r.Name,
		CrossSeeds: len(r.Hashes) - 1,
		Size:       fmt.Sprintf("%.1f GB", float64(r.SizeBytes)/(1<<30)),
		Seeded:     fmt.Sprintf("%.0f days", r.Seeded.Hours()/24),
		Rule:       r.Rule,
		Where:      r.ContentPath,
		KeptFor:    r.KeptFor,
	}
}

// errApplyBusy means another apply holds the mover lock - for a scheduled
// task, a reason to try again next tick rather than a failure.
var errApplyBusy = errors.New("another apply is running")

// removalOutcome is what one removal run did.
type removalOutcome struct {
	Removed  []torrents.Removal
	Failed   []string
	Deferred int
}

func (o removalOutcome) freedBytes() int64 {
	var n int64
	for _, r := range o.Removed {
		if r.DeleteFiles {
			n += r.SizeBytes
		}
	}
	return n
}

// removeFinishedSeeds removes every download done seeding that's safe to
// remove (see torrents.PlanRemovals), through qBittorrent - shared by the
// Downloads page's "Remove now" and the scheduled task. It takes the same
// gates and lock as an apply: removing frees space a plan may be counting
// on, so it never runs alongside one, nor while anything is still moving
// or a drive is missing.
func (s *Server) removeFinishedSeeds(eng *engine.Engine, now time.Time) (removalOutcome, error) {
	var out removalOutcome
	if eng.Cfg.Torrents.Removal.EffectiveMode() != config.RemovalRemove {
		return out, fmt.Errorf("removal isn't set to remove - see Settings > Downloads")
	}
	if !eng.DownloadsEnabled() {
		return out, fmt.Errorf("download tiering needs qBittorrent connected and at least one downloads tier")
	}
	if s.applyInFlight() {
		return out, fmt.Errorf("%w: an apply is running, and removals wait until it's finished", errApplyBusy)
	}
	if busy, detail, err := eng.MovesInFlight(); err != nil {
		return out, err
	} else if busy {
		return out, fmt.Errorf("refusing to remove anything: %s", detail)
	}
	if err := eng.CheckStorage(); err != nil {
		return out, err
	}

	lock, err := mover.AcquireLock(filepath.Dir(s.cfgPath))
	if err != nil {
		return out, fmt.Errorf("%w: %w", errApplyBusy, err)
	}
	defer func() { _ = lock.Release() }()

	inv, err := eng.BuildDownloadsInventory(now, true)
	if err != nil {
		return out, err
	}
	plan, err := eng.PlanRemovals(inv)
	if err != nil {
		return out, err
	}
	remover := eng.Remover()
	for _, r := range plan.Removals {
		if err := remover.Remove(r); err != nil {
			log.Printf("webui: removing %s: %v", r.Name, err)
			out.Failed = append(out.Failed, err.Error())
			continue
		}
		log.Printf("webui: removed %s from qBittorrent (%s), done seeding after %.0f days", r.Name, map[bool]string{true: "with its files", false: "files kept for " + r.KeptFor}[r.DeleteFiles], r.Seeded.Hours()/24)
		out.Removed = append(out.Removed, r)
	}
	for _, r := range plan.Deferred {
		out.Deferred += len(r.Hashes)
	}
	return out, nil
}

func (s *Server) handleDownloadsRemove(w http.ResponseWriter, r *http.Request) {
	eng, err := s.newEngine()
	var out removalOutcome
	if err == nil {
		out, err = s.removeFinishedSeeds(eng, time.Now())
	}
	data := s.buildDownloadsPageData()
	if err != nil {
		data.RemoveError = err.Error()
	} else {
		data.Removed = fmt.Sprintf("Removed %d download(s), freeing %.1f GB.", len(out.Removed), float64(out.freedBytes())/(1<<30))
		if len(out.Failed) > 0 {
			data.RemoveError = fmt.Sprintf("%d couldn't be removed: %s", len(out.Failed), strings.Join(out.Failed, "; "))
		}
	}
	if eng != nil && len(out.Removed) > 0 {
		s.notifyRemovals(out, false)
	}
	s.render(w, "downloads", data)
}

// notifyRemovals sends the summary every removal run sends - regardless of
// verbose: removing downloads is never something to find out about later.
func (s *Server) notifyRemovals(out removalOutcome, scheduled bool) {
	n := s.notifier()
	title := "Finished seeds removed"
	if scheduled {
		title = "Scheduled removal of finished seeds"
	}
	level := notify.LevelSuccess
	if len(out.Failed) > 0 {
		level = notify.LevelWarning
	}
	var names []string
	for _, r := range out.Removed {
		names = append(names, n.Code(r.Name))
	}
	body := fmt.Sprintf("removed %s download(s), freeing %s, failed %s", n.Bold(strconv.Itoa(len(out.Removed))), n.Bold(fmt.Sprintf("%.1f GB", float64(out.freedBytes())/(1<<30))), n.Bold(strconv.Itoa(len(out.Failed))))
	if len(names) > 0 {
		body += ": " + n.JoinLines(names)
	}
	if out.Deferred > 0 {
		body += fmt.Sprintf(" - %d more wait for the next run (per-run cap)", out.Deferred)
	}
	n.Summary(title, body, level)
	for _, f := range out.Failed {
		n.Item("Failed to remove a download", n.Code(f), notify.LevelFailure)
	}
}

// runScheduledExpireSeeds is the "Remove Finished Seeds" task: refresh
// trackers (best-effort), then report or remove what's done seeding,
// according to the removal mode. Only called from tick() when
// scheduler.Due says it's time.
func (s *Server) runScheduledExpireSeeds(now time.Time) {
	if s.applyInFlight() {
		log.Printf("scheduler: expire-seeds is due, but an apply is in flight - skipping this tick, will retry next tick")
		return
	}
	n := s.notifier()
	eng, err := s.newEngine()
	if err != nil {
		log.Printf("scheduler: expire-seeds: %v", err)
		n.Summary("Removing finished seeds failed", err.Error(), notify.LevelFailure)
		s.recordTaskRan(taskExpireSeeds, now)
		return
	}
	mode := eng.Cfg.Torrents.Removal.EffectiveMode()
	if mode == config.RemovalOff {
		s.recordTaskRan(taskExpireSeeds, now)
		n.Summary("Finished seeds: removal is off", "Nothing was checked - turn removal on, or report-only, under Settings > Downloads.", notify.LevelInfo)
		return
	}
	if !eng.DownloadsEnabled() {
		s.recordTaskRan(taskExpireSeeds, now)
		n.Summary("Removing finished seeds failed", "download tiering needs qBittorrent connected and at least one downloads tier", notify.LevelFailure)
		return
	}

	// Trackers decide what's done, so refresh them first - best-effort,
	// as with the plan's quality-cutoff scan: a scan already running, or
	// failing, leaves what's already known.
	if s.scanTrackersMu.TryLock() {
		if _, err := eng.ScanTrackers(now); err != nil {
			log.Printf("scheduler: expire-seeds: tracker scan failed, going on with what's known: %v", err)
		} else {
			s.recordTaskRan(taskScanTrackers, now)
		}
		s.scanTrackersMu.Unlock()
	}

	if mode == config.RemovalReport {
		s.recordTaskRan(taskExpireSeeds, now)
		inv, err := eng.BuildDownloadsInventory(now, true)
		var plan torrents.RemovalPlan
		if err == nil {
			plan, err = eng.PlanRemovals(inv)
		}
		if err != nil {
			log.Printf("scheduler: expire-seeds: %v", err)
			n.Summary("Checking finished seeds failed", err.Error(), notify.LevelFailure)
			return
		}
		var names []string
		var total int64
		for _, r := range append(plan.Removals, plan.Deferred...) {
			names = append(names, n.Code(r.Name))
			if r.DeleteFiles {
				total += r.SizeBytes
			}
		}
		body := fmt.Sprintf("%s download(s) would be removed, freeing %s - report only, nothing was removed", n.Bold(strconv.Itoa(len(names))), n.Bold(fmt.Sprintf("%.1f GB", float64(total)/(1<<30))))
		if len(names) > 0 {
			body += ": " + n.JoinLines(names)
		}
		n.Summary("Finished seeds (report only)", body, notify.LevelInfo)
		return
	}

	if busy, detail, err := eng.MovesInFlight(); err != nil {
		log.Printf("scheduler: expire-seeds: %v - skipping this tick", err)
		return
	} else if busy {
		log.Printf("scheduler: expire-seeds is due, but %s - skipping this tick, will retry next tick", detail)
		return
	}
	out, err := s.removeFinishedSeeds(eng, now)
	if err != nil {
		if errors.Is(err, errApplyBusy) {
			log.Printf("scheduler: expire-seeds: %v - skipping this tick", err)
			return
		}
		log.Printf("scheduler: expire-seeds: %v", err)
		n.Summary("Removing finished seeds failed", err.Error(), notify.LevelFailure)
		s.recordTaskRan(taskExpireSeeds, now)
		return
	}
	s.recordTaskRan(taskExpireSeeds, now)
	s.notifyRemovals(out, true)
}
