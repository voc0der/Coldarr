package webui

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
}

type downloadsPageData struct {
	Title  string
	Run    applyStatusData
	Plan   downloadsPlanView
	Recent []downloadEventView
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
