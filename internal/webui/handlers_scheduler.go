package webui

import (
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
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
	return &notify.Notifier{URL: conn.URL, Verbose: cfg.Notifications.Verbose}
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
}

type schedulerData struct {
	Title      string
	Saved      string
	RunPlan    scheduleFormView
	RescanCold scheduleFormView
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
			`Builds a fresh plan using your current policy and executes it - identical to clicking "Apply this plan" yourself, just unattended.`,
			cfg.Scheduler.RunPlan, s.getLastRanPlan()),
		RescanCold: s.scheduleView("rescan_cold", "Rescan Cold Storage",
			"Refreshes disk usage and Radarr/Sonarr's library for your cold tiers only, and reports what it finds - a health check, not a move.",
			cfg.Scheduler.RescanCold, s.getLastRanRescan()),
	}
}

func (s *Server) handleSchedulerPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings_scheduler", s.schedulerData())
}

func (s *Server) handleSchedulerSave(w http.ResponseWriter, r *http.Request) {
	task := r.PathValue("task")
	if task != "run_plan" && task != "rescan_cold" {
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
		n.Summary("Scheduled apply finished", fmt.Sprintf("moved %d, failed %d", len(moved), len(failed)), level)

		for _, e := range moved {
			n.Item("Moved "+e.Entry.Item.Title, fmt.Sprintf("to %s (%s)", e.Entry.ToTier, e.Entry.ToPath), notify.LevelSuccess)
		}
		for _, e := range failed {
			n.Item("Failed to move "+e.Entry.Item.Title, e.Err, notify.LevelFailure)
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
			lines = append(lines, fmt.Sprintf("%s (%s): %v", status.Tier.Name, status.Path, status.Err))
			n.Item(status.Tier.Name+" unavailable", fmt.Sprintf("%s: %v", status.Path, status.Err), notify.LevelFailure)
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%s): %.1f%% used", status.Tier.Name, status.Path, status.Usage.UsedPercent))
		n.Item(status.Tier.Name, fmt.Sprintf("%.1f%% used", status.Usage.UsedPercent), notify.LevelInfo)
	}

	title, level := "Cold storage check finished", notify.LevelSuccess
	if failures > 0 {
		title, level = fmt.Sprintf("Cold storage check: %d issue(s)", failures), notify.LevelWarning
	}
	n.Summary(title, strings.Join(lines, "; "), level)
}
