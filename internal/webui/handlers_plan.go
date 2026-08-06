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
	Links []linkView
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

	// A plan computed while Radarr/Sonarr are still physically executing
	// earlier move commands is built from mid-move disk numbers - it will
	// happily propose stuffing a drive whose real free space is already
	// spoken for. Refuse to show one at all rather than show one with a
	// caveat: an operator (or the scheduler) acting on it is exactly how
	// destinations get overfilled.
	if busy, detail, err := eng.ArrMovesInFlight(); err != nil {
		data.Error = err.Error()
		return data
	} else if busy {
		data.Error = detail
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

	linkSrc := s.buildLinkSources()

	var total int64
	for _, e := range plan.Entries {
		total += e.Item.SizeBytes
		why := ""
		if len(e.Reasons) > 0 {
			why = e.Reasons[0]
		}
		data.Entries = append(data.Entries, planEntryView{
			Title: e.Item.Title,
			Links: itemLinks(linkSrc, e.Item.ArrApp, e.Item.TitleSlug, e.Item.Path),
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
	// The flock below only guards against a concurrently *running* Coldarr
	// apply - it dies with Coldarr's process, while move commands already
	// handed to Radarr/Sonarr keep physically copying on their own. This
	// check is what actually prevents starting a new run on top of a
	// previous run's still-in-flight moves after a crash or restart. A
	// failed check refuses the apply rather than assuming idle.
	if busy, detail, err := eng.ArrMovesInFlight(); err != nil {
		return nil, err
	} else if busy {
		return nil, fmt.Errorf("refusing to apply: %s", detail)
	}

	lock, err := mover.AcquireLock(filepath.Dir(s.cfgPath))
	if err != nil {
		return nil, err
	}

	progress := eng.Movers().Apply(plan, inv.VolumeOf())

	s.applyMu.Lock()
	s.currentRun = &applyRun{progress: progress, lock: lock}
	s.applyMu.Unlock()

	go func() {
		defer func() { _ = lock.Release() }()
		progress.Wait()

		if moved := progress.Snapshot().Moved(); len(moved) > 0 {
			if err := eng.NotifyJellyfinMoved(moved); err != nil {
				log.Printf("webui: jellyfin update after apply failed: %v", err)
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
	Finished    string
	MovedCount  int
	FailedCount int
	Entries     []applyStatusEntryView
}

// finishedResultTTL bounds how long a completed apply's result stays
// pinned to the top of the Plan page. Past that, it's stale news rather
// than something worth pushing the fresh dry-run preview down for - the
// page reverts to looking exactly like one that's never had an apply run.
// A var, not a const, so tests can shrink it instead of sleeping an hour.
var finishedResultTTL = time.Hour

func (s *Server) currentApplyStatus() applyStatusData {
	s.applyMu.Lock()
	run := s.currentRun
	s.applyMu.Unlock()

	if run == nil {
		return applyStatusData{NoRun: true}
	}

	snap := run.progress.Snapshot()
	if snap.Done && time.Since(snap.Finished) > finishedResultTTL {
		return applyStatusData{NoRun: true}
	}

	data := applyStatusData{Running: !snap.Done}
	if snap.Done {
		data.Finished = snap.Finished.Format("2006-01-02 15:04")
	}

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
