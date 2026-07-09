package webui

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/vocoder/coldarr/internal/notify"
	"github.com/vocoder/coldarr/internal/scheduler"
)

// notifier builds a fresh *notify.Notifier from the current Apprise URL
// (in connStore, not coldarr.yaml) and Verbose flag - built fresh on every
// use, like newEngine(), so a Notifications save takes effect without a
// restart.
func (s *Server) notifier() *notify.Notifier {
	conn, _ := s.connStore.Get("apprise")
	cfg := s.currentConfig()
	return &notify.Notifier{URL: conn.URL, Verbose: cfg.Notifications.Verbose, Markdown: cfg.Notifications.Markdown, Tag: cfg.Notifications.Tag}
}

type scheduleFormView struct {
	Task    string
	Label   string
	Hint    string
	Enabled bool
	Unit    string
	Every   int
	At      string
	LastRan string
	Error   string
	// ShowRunNow adds a button below the schedule form (labeled
	// RunNowLabel) - only set for tasks where waiting out a full
	// schedule period before ever seeing a result is a poor first-run
	// experience (refresh_links: the Links column would otherwise stay
	// empty; scan_cutoffs: quality-cutoff protection would otherwise
	// silently sit inactive until the schedule fires; scan_orphans: the
	// Orphaned Storage page would otherwise stay empty).
	ShowRunNow  bool
	RunNowLabel string
}

type schedulerData struct {
	Title        string
	Saved        string
	RunPlan      scheduleFormView
	RescanCold   scheduleFormView
	RefreshLinks scheduleFormView
	ScanCutoffs  scheduleFormView
	ScanOrphans  scheduleFormView
}

func (s *Server) scheduleView(task, label, hint string, sched scheduler.Schedule, lastRun time.Time) scheduleFormView {
	view := scheduleFormView{
		Task:    task,
		Label:   label,
		Hint:    hint,
		Enabled: sched.Enabled,
		Unit:    string(sched.Unit),
		Every:   sched.Every,
		At:      sched.At,
	}
	switch task {
	case "refresh_links":
		view.ShowRunNow, view.RunNowLabel = true, "Refresh now"
	case "scan_cutoffs":
		view.ShowRunNow, view.RunNowLabel = true, "Scan now"
	case "scan_orphans":
		view.ShowRunNow, view.RunNowLabel = true, "Scan now"
	}
	if !lastRun.IsZero() {
		view.LastRan = lastRun.Format("2006-01-02 15:04")
	}
	return view
}

func (s *Server) schedulerData() schedulerData {
	cfg := s.currentConfig()
	return schedulerData{
		Title: "Scheduler",
		RunPlan: s.scheduleView("run_plan", "Run the Plan",
			`Builds a fresh plan using your current policy and executes it - identical to clicking "Apply this plan" yourself, just unattended. Always scans quality cutoffs first (see "Scan Quality Cutoffs" below) so it acts on current data, even if that schedule is off.`,
			cfg.Scheduler.RunPlan, s.getLastRanPlan()),
		RescanCold: s.scheduleView("rescan_cold", "Rescan Cold Storage",
			"Refreshes disk usage and Radarr/Sonarr's library for your cold tiers only, and reports what it finds - a health check, not a move.",
			cfg.Scheduler.RescanCold, s.getLastRanRescan()),
		RefreshLinks: s.scheduleView("refresh_links", "Refresh Links Cache",
			`Refreshes the Radarr/Sonarr/Jellyfin lookups behind the Plan/History pages' Links column, so opening those pages never waits on a live Jellyfin (and, for History, Radarr/Sonarr) call. This is reference data that almost never changes - until this has run at least once (or you click "Refresh now" below), the Links column just won't show anything yet.`,
			cfg.Scheduler.RefreshLinks, s.getLastRanRefreshLinks()),
		ScanCutoffs: s.scheduleView("scan_cutoffs", "Scan Quality Cutoffs",
			`Checks which movies/series have a file that doesn't meet its quality profile's upgrade cutoff, so scoring can keep those on hot storage (Radarr/Sonarr are still going to replace that file). This calls Radarr/Sonarr's own wanted/cutoff lookup, which can be slow on a large library - that's exactly why it only ever runs here, on a schedule (or via "Scan now" below), never live on a Dashboard/Plan page view. Until this has run at least once, every item is treated as cutoff-met.`,
			cfg.Scheduler.ScanCutoffs, s.getLastRanScanCutoffs()),
		ScanOrphans: s.scheduleView("scan_orphans", "Scan for Orphaned Storage",
			`Walks your configured tier paths on disk looking for folders that no longer correspond to anything Radarr, Sonarr, or Jellyfin still tracks, and reports them on the Orphaned Storage page (linked from Storage tiers). This is a filesystem walk, which can be slow on a large or slow cold tier - that's why it only ever runs here, on a schedule (or via "Scan now" below), never live on a page view.`,
			cfg.Scheduler.ScanOrphans, s.getLastRanScanOrphans()),
	}
}

func (s *Server) handleSchedulerPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings_scheduler", s.schedulerData())
}

func (s *Server) handleSchedulerSave(w http.ResponseWriter, r *http.Request) {
	task := r.PathValue("task")
	if task != "run_plan" && task != "rescan_cold" && task != "refresh_links" && task != "scan_cutoffs" && task != "scan_orphans" {
		http.NotFound(w, r)
		return
	}

	_ = r.ParseForm()
	sched := scheduler.Schedule{
		Enabled: r.FormValue("enabled") == "on",
		Unit:    scheduler.Unit(r.FormValue("unit")),
		Every:   parseIntOrZero(r.FormValue("every")),
		At:      r.FormValue("at"),
	}

	if err := s.updateSchedule(task, sched); err != nil {
		data := s.schedulerData()
		submitted := scheduleFormView{
			Task: task, Enabled: sched.Enabled, Unit: string(sched.Unit), Every: sched.Every, At: sched.At,
			Error: err.Error(),
		}
		switch task {
		case "run_plan":
			submitted.Label, submitted.Hint, submitted.LastRan = data.RunPlan.Label, data.RunPlan.Hint, data.RunPlan.LastRan
			data.RunPlan = submitted
		case "rescan_cold":
			submitted.Label, submitted.Hint, submitted.LastRan = data.RescanCold.Label, data.RescanCold.Hint, data.RescanCold.LastRan
			data.RescanCold = submitted
		case "refresh_links":
			submitted.Label, submitted.Hint, submitted.LastRan = data.RefreshLinks.Label, data.RefreshLinks.Hint, data.RefreshLinks.LastRan
			submitted.ShowRunNow, submitted.RunNowLabel = true, "Refresh now"
			data.RefreshLinks = submitted
		case "scan_cutoffs":
			submitted.Label, submitted.Hint, submitted.LastRan = data.ScanCutoffs.Label, data.ScanCutoffs.Hint, data.ScanCutoffs.LastRan
			submitted.ShowRunNow, submitted.RunNowLabel = true, "Scan now"
			data.ScanCutoffs = submitted
		case "scan_orphans":
			submitted.Label, submitted.Hint, submitted.LastRan = data.ScanOrphans.Label, data.ScanOrphans.Hint, data.ScanOrphans.LastRan
			submitted.ShowRunNow, submitted.RunNowLabel = true, "Scan now"
			data.ScanOrphans = submitted
		}
		s.render(w, "settings_scheduler", data)
		return
	}

	data := s.schedulerData()
	data.Saved = "Schedule saved."
	s.render(w, "settings_scheduler", data)
}

func parseIntOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// runScheduledPlan is the scheduled counterpart to handleApplyStart: build
// a fresh plan using whatever policy is currently configured and execute
// it unattended - identical effect to a manual "Apply this plan" click.
// Only called from tick() when scheduler.Due says it's time.
func (s *Server) runScheduledPlan(now time.Time) {
	s.applyMu.Lock()
	inFlight := s.currentRun != nil && !s.currentRun.progress.Snapshot().Done
	s.applyMu.Unlock()
	if inFlight {
		log.Printf("scheduler: run-plan is due, but an apply is already in flight - skipping this tick, will retry next tick")
		return
	}

	n := s.notifier()

	eng, err := s.newEngine()
	if err != nil {
		log.Printf("scheduler: run-plan: %v", err)
		n.Summary("Scheduled apply failed", err.Error(), notify.LevelFailure)
		s.recordPlanRan(now)
		return
	}

	// Same reasoning as the in-flight guard above, but for moves running
	// inside Radarr/Sonarr themselves - which survive Coldarr restarts,
	// making them invisible to currentRun. Skip without recordPlanRan so
	// the tick retries once they've drained, exactly like the guard above;
	// startApply would also refuse, but checking here avoids building (and
	// half-acting on) a plan from mid-move disk numbers at all.
	if busy, detail, err := eng.ArrMovesInFlight(); err != nil {
		log.Printf("scheduler: run-plan: %v - skipping this tick", err)
		return
	} else if busy {
		log.Printf("scheduler: run-plan is due, but %s - skipping this tick, will retry next tick", detail)
		return
	}

	// Scan quality cutoffs first, so an actual unattended apply acts on
	// current data even if the "Scan Quality Cutoffs" schedule itself is
	// off. Best-effort: if another scan is already in flight, or this one
	// fails (e.g. Radarr/Sonarr hiccups on just this endpoint), log it and
	// proceed with whatever's already cached rather than failing the
	// whole scheduled apply over it.
	if s.scanCutoffsMu.TryLock() {
		if err := eng.CutoffCache.Refresh(eng.Radarr, eng.Sonarr); err != nil {
			log.Printf("scheduler: run-plan: pre-plan quality-cutoff scan failed, proceeding with cached data: %v", err)
		} else {
			s.recordScanCutoffsRan(now)
		}
		s.scanCutoffsMu.Unlock()
	} else {
		log.Printf("scheduler: run-plan: a quality-cutoff scan is already in progress, proceeding with cached data")
	}

	inv, err := eng.BuildInventory(now)
	if err != nil {
		log.Printf("scheduler: run-plan: %v", err)
		n.Summary("Scheduled apply failed", err.Error(), notify.LevelFailure)
		s.recordPlanRan(now)
		return
	}

	plan, err := eng.BuildPlan(inv, now)
	if err != nil {
		log.Printf("scheduler: run-plan: %v", err)
		n.Summary("Scheduled apply failed", err.Error(), notify.LevelFailure)
		s.recordPlanRan(now)
		return
	}

	if len(plan.Entries) == 0 {
		s.recordPlanRan(now)
		n.Summary("Scheduled apply: nothing to move", "No cold-eligible items found on hot storage with room to accept them.", notify.LevelInfo)
		return
	}

	progress, err := s.startApply(eng, inv, plan)
	if err != nil {
		// Most likely the mover lock is held by something else (a manual
		// apply that started between the inFlight check above and here,
		// or the CLI's `coldarr apply`) - skip this tick and retry next
		// minute, same as the inFlight guard above.
		log.Printf("scheduler: run-plan: %v", err)
		return
	}
	s.recordPlanRan(now)

	go func() {
		progress.Wait()
		snap := progress.Snapshot()
		moved, failed := snap.Moved(), snap.Failed()

		level := notify.LevelSuccess
		if len(failed) > 0 {
			level = notify.LevelWarning
		}
		n.Summary("Scheduled apply finished", fmt.Sprintf("moved %s, failed %s", n.Bold(strconv.Itoa(len(moved))), n.Bold(strconv.Itoa(len(failed)))), level)

		for _, e := range moved {
			n.Item("Moved "+e.Entry.Item.Title, fmt.Sprintf("to %s (%s)", n.Bold(e.Entry.ToTier), n.Code(e.Entry.ToPath)), notify.LevelSuccess)
		}
		for _, e := range failed {
			n.Item("Failed to move "+e.Entry.Item.Title, n.Code(e.Err), notify.LevelFailure)
		}
	}()
}

// runScheduledRescan re-runs the same disk-usage + Radarr/Sonarr fetch
// that already happens on every Plan page load (BuildInventory), scoped
// to reporting on cold tiers only, and sends a notification summary. It
// never touches the mover lock or applyMu - this is a read-only health
// check, not a move. Only called from tick() when scheduler.Due says it's
// time.
func (s *Server) runScheduledRescan(now time.Time) {
	if !s.rescanMu.TryLock() {
		log.Printf("scheduler: rescan-cold-storage is due, but a previous rescan is still running - skipping this tick")
		return
	}
	defer s.rescanMu.Unlock()

	s.recordRescanRan(now)
	n := s.notifier()

	eng, err := s.newEngine()
	if err != nil {
		log.Printf("scheduler: rescan-cold-storage: %v", err)
		n.Summary("Cold storage check failed", err.Error(), notify.LevelFailure)
		return
	}

	inv, err := eng.BuildInventory(now)
	if err != nil {
		log.Printf("scheduler: rescan-cold-storage: %v", err)
		n.Summary("Cold storage check failed", err.Error(), notify.LevelFailure)
		return
	}

	coldPaths := inv.ColdTierPaths()
	if len(coldPaths) == 0 {
		n.Summary("Cold storage check finished", "No cold tiers configured.", notify.LevelInfo)
		return
	}

	var lines []string
	failures := 0
	for _, status := range coldPaths {
		if status.Err != nil {
			failures++
			lines = append(lines, fmt.Sprintf("%s (%s): %s", n.Bold(status.Tier.Name), n.Code(status.Path), n.Code(status.Err.Error())))
			n.Item(status.Tier.Name+" unavailable", fmt.Sprintf("%s: %s", n.Code(status.Path), n.Code(status.Err.Error())), notify.LevelFailure)
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%s): %s used", n.Bold(status.Tier.Name), n.Code(status.Path), n.Bold(fmt.Sprintf("%.1f%%", status.Usage.UsedPercent))))
		n.Item(status.Tier.Name, fmt.Sprintf("%s used", n.Bold(fmt.Sprintf("%.1f%%", status.Usage.UsedPercent))), notify.LevelInfo)
	}

	title, level := "Cold storage check finished", notify.LevelSuccess
	if failures > 0 {
		title, level = fmt.Sprintf("Cold storage check: %d issue(s)", failures), notify.LevelWarning
	}
	n.Summary(title, n.JoinLines(lines), level)
}

// runScheduledRefreshLinks re-fetches the Radarr/Sonarr titleSlug and
// Jellyfin item-ID/server-ID lookups the Links column needs and persists
// them to the link cache (see internal/linkcache), so Plan/History page
// views can read them instead of hitting Radarr/Sonarr/Jellyfin live.
// Read-only from Radarr/Sonarr/Jellyfin's point of view and never touches
// the mover lock. Only called from tick() when scheduler.Due says it's
// time.
func (s *Server) runScheduledRefreshLinks(now time.Time) {
	if !s.refreshLinksMu.TryLock() {
		log.Printf("scheduler: refresh-links-cache is due, but a previous refresh is still running - skipping this tick")
		return
	}
	defer s.refreshLinksMu.Unlock()

	s.recordRefreshLinksRan(now)
	n := s.notifier()

	eng, err := s.newEngine()
	if err != nil {
		log.Printf("scheduler: refresh-links-cache: %v", err)
		n.Summary("Links cache refresh failed", err.Error(), notify.LevelFailure)
		return
	}

	if err := s.linkCache.Refresh(eng.Radarr, eng.Sonarr, eng.JellyfinClient()); err != nil {
		log.Printf("scheduler: refresh-links-cache: %v", err)
		n.Summary("Links cache refresh failed", err.Error(), notify.LevelFailure)
		return
	}

	n.Summary("Links cache refreshed", "Plan/History Links column lookups are up to date.", notify.LevelSuccess)
}

// handleRefreshLinksNow is the manual counterpart to runScheduledRefreshLinks
// - since the schedule (like every schedule in this app) defaults to off,
// and even once enabled deliberately waits out a full period before its
// first fire, an operator would otherwise see an empty Links column
// indefinitely unless they also knew to wait. This runs the exact same
// refresh synchronously and reports the result inline, and - like a
// genuine scheduled run - resets the due-check anchor so an already-
// enabled schedule doesn't immediately fire again right after this.
func (s *Server) handleRefreshLinksNow(w http.ResponseWriter, r *http.Request) {
	eng, err := s.newEngine()
	if err == nil {
		err = s.linkCache.Refresh(eng.Radarr, eng.Sonarr, eng.JellyfinClient())
	}

	if err != nil {
		data := s.schedulerData()
		data.RefreshLinks.Error = err.Error()
		s.render(w, "settings_scheduler", data)
		return
	}

	s.recordRefreshLinksRan(time.Now())
	data := s.schedulerData()
	data.Saved = "Links cache refreshed."
	s.render(w, "settings_scheduler", data)
}

// runScheduledScanCutoffs refreshes internal/cutoffcache - which movies/
// series have a file that doesn't meet its quality profile's upgrade
// cutoff - by calling Radarr/Sonarr's own wanted/cutoff lookup. This is
// the only place that lookup happens outside runScheduledPlan's own
// pre-plan scan and the manual "Scan now" button: never live on a
// Dashboard/Plan page view, since it's known to be slow on real-world
// libraries. Only called from tick() when scheduler.Due says it's time.
func (s *Server) runScheduledScanCutoffs(now time.Time) {
	if !s.scanCutoffsMu.TryLock() {
		log.Printf("scheduler: scan-quality-cutoffs is due, but a previous scan is still running - skipping this tick")
		return
	}
	defer s.scanCutoffsMu.Unlock()

	s.recordScanCutoffsRan(now)
	n := s.notifier()

	eng, err := s.newEngine()
	if err != nil {
		log.Printf("scheduler: scan-quality-cutoffs: %v", err)
		n.Summary("Quality-cutoff scan failed", err.Error(), notify.LevelFailure)
		return
	}

	if err := eng.CutoffCache.Refresh(eng.Radarr, eng.Sonarr); err != nil {
		log.Printf("scheduler: scan-quality-cutoffs: %v", err)
		n.Summary("Quality-cutoff scan failed", err.Error(), notify.LevelFailure)
		return
	}

	snap := eng.CutoffCache.Get()
	n.Summary("Quality-cutoff scan finished",
		fmt.Sprintf("%s movie(s), %s series with an unmet cutoff", n.Bold(strconv.Itoa(len(snap.RadarrUnmetIDs))), n.Bold(strconv.Itoa(len(snap.SonarrUnmetIDs)))),
		notify.LevelSuccess)
}

// handleScanCutoffsNow is the manual counterpart to runScheduledScanCutoffs
// - since the schedule (like every schedule in this app) defaults to off,
// an operator would otherwise have quality-cutoff protection sit inactive
// indefinitely unless they also knew to wait for it. This runs the exact
// same scan synchronously and reports the result inline, and - like a
// genuine scheduled run - resets the due-check anchor so an already-
// enabled schedule doesn't immediately fire again right after this.
func (s *Server) handleScanCutoffsNow(w http.ResponseWriter, r *http.Request) {
	if !s.scanCutoffsMu.TryLock() {
		data := s.schedulerData()
		data.ScanCutoffs.Error = "A scan is already in progress - try again shortly."
		s.render(w, "settings_scheduler", data)
		return
	}
	defer s.scanCutoffsMu.Unlock()

	eng, err := s.newEngine()
	if err == nil {
		err = eng.CutoffCache.Refresh(eng.Radarr, eng.Sonarr)
	}

	if err != nil {
		data := s.schedulerData()
		data.ScanCutoffs.Error = err.Error()
		s.render(w, "settings_scheduler", data)
		return
	}

	s.recordScanCutoffsRan(time.Now())
	data := s.schedulerData()
	data.Saved = "Quality-cutoff scan finished."
	s.render(w, "settings_scheduler", data)
}

// runScheduledScanOrphans refreshes internal/orphans - which folders on a
// configured tier path no service (Radarr, Sonarr, Jellyfin) still tracks
// - by walking every tier path on disk. This is the only place that walk
// happens besides the manual "Scan now" button: never live on a page
// view, since it's a filesystem walk that can be slow on a large or slow
// cold tier. Only called from tick() when scheduler.Due says it's time.
func (s *Server) runScheduledScanOrphans(now time.Time) {
	if !s.scanOrphansMu.TryLock() {
		log.Printf("scheduler: scan-orphaned-storage is due, but a previous scan is still running - skipping this tick")
		return
	}
	defer s.scanOrphansMu.Unlock()

	s.recordScanOrphansRan(now)
	n := s.notifier()

	eng, err := s.newEngine()
	if err != nil {
		log.Printf("scheduler: scan-orphaned-storage: %v", err)
		n.Summary("Orphaned storage scan failed", err.Error(), notify.LevelFailure)
		return
	}

	if err := s.orphanStore.Refresh(eng.Radarr, eng.Sonarr, eng.JellyfinClient(), eng.Cfg.Tiers); err != nil {
		log.Printf("scheduler: scan-orphaned-storage: %v", err)
		n.Summary("Orphaned storage scan failed", err.Error(), notify.LevelFailure)
		return
	}

	snap := s.orphanStore.Get()
	n.Summary("Orphaned storage scan finished",
		fmt.Sprintf("%s folder(s) found with no matching service record", n.Bold(strconv.Itoa(len(snap.Candidates)))),
		notify.LevelSuccess)
}

// scanOrphansNow acquires scanOrphansMu (like a scheduled run, best-effort
// via TryLock so two manual clicks - or a manual click racing a scheduled
// tick - can't scan concurrently), runs the scan synchronously, and
// records it like a genuine run on success. Shared by both places an
// operator can trigger this by hand: the Scheduler settings page's
// "Scan now" (handleScanOrphansNow) and the Orphaned Storage page's own
// convenience button (handleOrphansScanNow) - each renders its own page
// afterward rather than duplicating this logic.
func (s *Server) scanOrphansNow() error {
	if !s.scanOrphansMu.TryLock() {
		return fmt.Errorf("a scan is already in progress - try again shortly")
	}
	defer s.scanOrphansMu.Unlock()

	eng, err := s.newEngine()
	if err == nil {
		err = s.orphanStore.Refresh(eng.Radarr, eng.Sonarr, eng.JellyfinClient(), eng.Cfg.Tiers)
	}
	if err != nil {
		return err
	}

	s.recordScanOrphansRan(time.Now())
	return nil
}

// handleScanOrphansNow is the Scheduler settings page's manual counterpart
// to runScheduledScanOrphans - since the schedule (like every schedule in
// this app) defaults to off, the Orphaned Storage page would otherwise
// stay empty indefinitely unless an operator also knew to wait.
func (s *Server) handleScanOrphansNow(w http.ResponseWriter, r *http.Request) {
	if err := s.scanOrphansNow(); err != nil {
		data := s.schedulerData()
		data.ScanOrphans.Error = err.Error()
		s.render(w, "settings_scheduler", data)
		return
	}

	data := s.schedulerData()
	data.Saved = "Orphaned storage scan finished."
	s.render(w, "settings_scheduler", data)
}
