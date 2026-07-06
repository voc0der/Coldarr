package webui

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/vocoder/coldarr/internal/engine"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/planner"
)

type planEntryView struct {
	Title string
	Type  string
	Size  string
	From  string
	To    string
	Score float64
	Why   string
}

type usageRowView struct {
	Tier   string
	Path   string
	Before float64
	After  float64
}

type planData struct {
	Error     string
	Empty     bool
	Entries   []planEntryView
	TotalSize string
	Usage     []usageRowView
	Warnings  []string
}

func (s *Server) buildPlanData() planData {
	var data planData

	eng, err := s.newEngine()
	if err != nil {
		data.Error = err.Error()
		return data
	}

	now := time.Now()
	inv, err := eng.BuildInventory(now)
	if err != nil {
		data.Error = err.Error()
		return data
	}

	plan, err := eng.BuildPlan(inv, now)
	if err != nil {
		data.Error = err.Error()
		return data
	}

	data.Empty = len(plan.Entries) == 0
	data.Warnings = append(append([]string{}, inv.Warnings...), plan.Warnings...)

	var total int64
	for _, e := range plan.Entries {
		total += e.Item.SizeBytes
		why := ""
		if len(e.Reasons) > 0 {
			why = e.Reasons[0]
		}
		data.Entries = append(data.Entries, planEntryView{
			Title: e.Item.Title,
			Type:  string(e.Item.Type),
			Size:  fmt.Sprintf("%.1f GB", float64(e.Item.SizeBytes)/(1<<30)),
			From:  fmt.Sprintf("%s (%s)", e.FromTier, e.FromPath),
			To:    fmt.Sprintf("%s (%s)", e.ToTier, e.ToPath),
			Score: e.Score,
			Why:   why,
		})
	}
	data.TotalSize = fmt.Sprintf("%.1f GB", float64(total)/(1<<30))

	before := inv.UsableUsage()
	for _, tier := range inv.Tiers {
		for _, path := range tier.Paths {
			b, ok := before[path]
			if !ok {
				continue
			}
			a := plan.FinalUsage[path]
			if fmt.Sprintf("%.1f", b.UsedPercent) == fmt.Sprintf("%.1f", a.UsedPercent) {
				continue
			}
			data.Usage = append(data.Usage, usageRowView{Tier: tier.Name, Path: path, Before: b.UsedPercent, After: a.UsedPercent})
		}
	}

	return data
}

// planPageData is the single /plan page's view model. Run is always
// computed; Plan is only populated when no apply is currently in flight -
// showing a dry-run preview next to live apply progress would be actively
// misleading, since the numbers wouldn't match what's really happening.
type planPageData struct {
	Title string
	Run   applyStatusData
	Plan  planData
}

func (s *Server) buildPlanPageData() planPageData {
	data := planPageData{Title: "Plan", Run: s.currentApplyStatus()}
	if !data.Run.Running {
		data.Plan = s.buildPlanData()
	}
	return data
}

func (s *Server) handlePlanPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "plan", s.buildPlanPageData())
}

// renderPlanError re-renders /plan with an error slotted into the dry-run
// section, preserving whatever the current apply run's status is.
func (s *Server) renderPlanError(w http.ResponseWriter, err error) {
	s.render(w, "plan", planPageData{
		Title: "Plan",
		Run:   s.currentApplyStatus(),
		Plan:  planData{Error: err.Error()},
	})
}

// handleApplyStart builds a fresh plan and starts executing it in the
// background (one move at a time per destination volume - see
// internal/mover), then redirects to the live status page. It never
// blocks on the moves themselves.
func (s *Server) handleApplyStart(w http.ResponseWriter, r *http.Request) {
	s.applyMu.Lock()
	if s.currentRun != nil && !s.currentRun.progress.Snapshot().Done {
		s.applyMu.Unlock()
		http.Redirect(w, r, "/plan", http.StatusSeeOther)
		return
	}
	s.applyMu.Unlock()

	eng, err := s.newEngine()
	if err != nil {
		s.renderPlanError(w, err)
		return
	}

	now := time.Now()
	inv, err := eng.BuildInventory(now)
	if err != nil {
		s.renderPlanError(w, err)
		return
	}

	plan, err := eng.BuildPlan(inv, now)
	if err != nil {
		s.renderPlanError(w, err)
		return
	}

	if len(plan.Entries) == 0 {
		http.Redirect(w, r, "/plan", http.StatusSeeOther)
		return
	}

	if _, err := s.startApply(eng, inv, plan); err != nil {
		s.renderPlanError(w, err)
		return
	}

	http.Redirect(w, r, "/plan", http.StatusSeeOther)
}

// startApply acquires the mover lock, kicks off the plan's moves in the
// background, and records the run as currentRun so the status page (and
// a concurrent scheduled tick, see handlers_scheduler.go) can see it's in
// flight - shared by the manual "Apply this plan" button and the
// scheduled "Run the Plan" task. It always spawns a goroutine that waits
// for the moves to finish and triggers a Jellyfin refresh if anything
// moved. A caller that also needs to know when the run finishes (e.g. to
// send a notification) calls progress.Wait() again itself -
// mover.Progress supports any number of independent waiters.
func (s *Server) startApply(eng *engine.Engine, inv *engine.Inventory, plan *planner.Plan) (*mover.Progress, error) {
	lock, err := mover.AcquireLock(filepath.Dir(s.cfgPath))
	if err != nil {
		return nil, err
	}

	progress := eng.Movers().Apply(plan, inv.VolumeOf())

	s.applyMu.Lock()
	s.currentRun = &applyRun{progress: progress, lock: lock}
	s.applyMu.Unlock()

	go func() {
		defer lock.Release()
		progress.Wait()

		if len(progress.Snapshot().Moved()) > 0 {
			if jf := eng.JellyfinClient(); jf != nil {
				if err := jf.RefreshLibrary(); err != nil {
					log.Printf("webui: jellyfin refresh after apply failed: %v", err)
				}
			}
		}
	}()

	return progress, nil
}

type applyStatusEntryView struct {
	Title  string
	To     string
	Status string
	Err    string
}

type applyStatusData struct {
	NoRun       bool
	Running     bool
	MovedCount  int
	FailedCount int
	Entries     []applyStatusEntryView
}

func (s *Server) currentApplyStatus() applyStatusData {
	s.applyMu.Lock()
	run := s.currentRun
	s.applyMu.Unlock()

	if run == nil {
		return applyStatusData{NoRun: true}
	}

	snap := run.progress.Snapshot()
	data := applyStatusData{Running: !snap.Done}

	for _, e := range snap.Entries {
		data.Entries = append(data.Entries, applyStatusEntryView{
			Title:  e.Entry.Item.Title,
			To:     fmt.Sprintf("%s (%s)", e.Entry.ToTier, e.Entry.ToPath),
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

// handleApplyStatusPartial serves the htmx-polled apply-status fragment.
// Once the run is no longer active, it tells htmx to reload the whole
// page instead of swapping just this fragment - hx-swap="outerHTML" only
// ever replaces the #apply-status div, so without this an operator
// watching the live page would see the status freeze at "finished" but
// never see the fresh dry-run preview section appear until a manual
// reload. HX-Refresh makes the server the single source of truth for
// what the merged /plan page looks like in every state.
func (s *Server) handleApplyStatusPartial(w http.ResponseWriter, r *http.Request) {
	data := s.currentApplyStatus()
	if !data.Running {
		w.Header().Set("HX-Refresh", "true")
	}
	s.renderPartial(w, "plan", "apply_status_table", data)
}
