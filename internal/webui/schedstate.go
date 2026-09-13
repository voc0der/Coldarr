package webui

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/scheduler"
)

// Scheduler task keys - the names the Scheduler settings form posts, and
// the keys of the persisted scheduler state.
const (
	taskRunPlan      = "run_plan"
	taskRescanCold   = "rescan_cold"
	taskRefreshLinks = "refresh_links"
	taskScanCutoffs  = "scan_cutoffs"
	taskScanOrphans  = "scan_orphans"
)

var scheduledTasks = []string{taskRunPlan, taskRescanCold, taskRefreshLinks, taskScanCutoffs, taskScanOrphans}

func scheduleFor(cfg *config.Config, task string) scheduler.Schedule {
	switch task {
	case taskRunPlan:
		return cfg.Scheduler.RunPlan
	case taskRescanCold:
		return cfg.Scheduler.RescanCold
	case taskRefreshLinks:
		return cfg.Scheduler.RefreshLinks
	case taskScanCutoffs:
		return cfg.Scheduler.ScanCutoffs
	case taskScanOrphans:
		return cfg.Scheduler.ScanOrphans
	}
	return scheduler.Schedule{}
}

// taskRunState is one task's scheduler timing, persisted so a restart
// never changes when a task next runs.
type taskRunState struct {
	// Anchor is what scheduler.Due compares against. A genuine run resets
	// it, and so can arming (process start, or saving the schedule) via
	// scheduler.Arm - which only ever moves it when the task would
	// otherwise fire on the spot.
	Anchor time.Time `json:"anchor,omitzero"`
	// LastRan is the user-facing "last ran" fact on the Scheduler settings
	// page. Unlike Anchor it's only ever set by a genuine run, so the page
	// never claims a task ran when it was really just armed or edited.
	LastRan time.Time `json:"last_ran,omitzero"`
}

// schedulerStatePath derives the scheduler state file's location the same
// way linkCachePath does - alongside the config file, not a separately
// configurable path.
func schedulerStatePath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "coldarr-scheduler.json")
}

// loadSchedulerState reads the scheduler state file at path, starting
// empty (every task never run) if it does not yet exist.
func loadSchedulerState(path string) (map[string]taskRunState, error) {
	state := map[string]taskRunState{}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return nil, fmt.Errorf("reading scheduler state %s: %w", path, err)
	}
	if len(data) == 0 {
		return state, nil
	}

	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parsing scheduler state %s: %w", path, err)
	}
	return state, nil
}

// saveSchedulerStateLocked must be called with schedMu held. A failed write
// is logged rather than returned: the in-memory state is still correct for
// this process's lifetime, and a scheduled task that genuinely ran must not
// be reported as failed just because its timestamp couldn't be persisted.
func (s *Server) saveSchedulerStateLocked() {
	if s.schedStatePath == "" {
		return
	}
	if err := writeSchedulerState(s.schedStatePath, s.schedState); err != nil {
		log.Printf("scheduler: %v - a restart may change when tasks next run", err)
	}
}

func writeSchedulerState(path string, state map[string]taskRunState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding scheduler state: %w", err)
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating scheduler state directory %s: %w", dir, err)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving %s: %w", path, err)
	}
	return nil
}

func (s *Server) taskState(task string) taskRunState {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.schedState[task]
}

// armTask re-arms task's due-check anchor as of now without recording a
// genuine run - see scheduler.Arm.
func (s *Server) armTask(task string, sched scheduler.Schedule, now time.Time) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if s.schedState == nil {
		s.schedState = map[string]taskRunState{}
	}
	st := s.schedState[task]
	armed := scheduler.Arm(sched, st.Anchor, now)
	if armed.Equal(st.Anchor) {
		return
	}
	st.Anchor = armed
	s.schedState[task] = st
	s.saveSchedulerStateLocked()
}

// recordTaskRan records that task genuinely executed at t - resets the
// due-check anchor (so it isn't considered due again until its next
// scheduled time) and updates the "last ran" fact shown on the Scheduler
// settings page.
func (s *Server) recordTaskRan(task string, t time.Time) {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if s.schedState == nil {
		s.schedState = map[string]taskRunState{}
	}
	s.schedState[task] = taskRunState{Anchor: t, LastRan: t}
	s.saveSchedulerStateLocked()
}

func (s *Server) getLastRunPlan() time.Time         { return s.taskState(taskRunPlan).Anchor }
func (s *Server) getLastRunRescan() time.Time       { return s.taskState(taskRescanCold).Anchor }
func (s *Server) getLastRunRefreshLinks() time.Time { return s.taskState(taskRefreshLinks).Anchor }
func (s *Server) getLastRunScanCutoffs() time.Time  { return s.taskState(taskScanCutoffs).Anchor }
func (s *Server) getLastRunScanOrphans() time.Time  { return s.taskState(taskScanOrphans).Anchor }

func (s *Server) getLastRanPlan() time.Time         { return s.taskState(taskRunPlan).LastRan }
func (s *Server) getLastRanRescan() time.Time       { return s.taskState(taskRescanCold).LastRan }
func (s *Server) getLastRanRefreshLinks() time.Time { return s.taskState(taskRefreshLinks).LastRan }
func (s *Server) getLastRanScanCutoffs() time.Time  { return s.taskState(taskScanCutoffs).LastRan }
func (s *Server) getLastRanScanOrphans() time.Time  { return s.taskState(taskScanOrphans).LastRan }

func (s *Server) recordPlanRan(t time.Time)         { s.recordTaskRan(taskRunPlan, t) }
func (s *Server) recordRescanRan(t time.Time)       { s.recordTaskRan(taskRescanCold, t) }
func (s *Server) recordRefreshLinksRan(t time.Time) { s.recordTaskRan(taskRefreshLinks, t) }
func (s *Server) recordScanCutoffsRan(t time.Time)  { s.recordTaskRan(taskScanCutoffs, t) }
func (s *Server) recordScanOrphansRan(t time.Time)  { s.recordTaskRan(taskScanOrphans, t) }
