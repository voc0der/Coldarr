package webui

import (
	"fmt"
	"net/http"
	"time"
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
	Title     string
	Error     string
	Empty     bool
	Entries   []planEntryView
	TotalSize string
	Usage     []usageRowView
	Warnings  []string
}

func (s *Server) buildPlanData() planData {
	data := planData{Title: "Plan"}

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

func (s *Server) handlePlanPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "plan", s.buildPlanData())
}

type failedMoveView struct {
	Title string
	Err   string
}

type applyResultView struct {
	Title             string
	Error             string
	Moved             int
	Failed            []failedMoveView
	JellyfinRefreshed bool
	JellyfinError     string
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	eng, err := s.newEngine()
	if err != nil {
		s.render(w, "apply_result", applyResultView{Title: "Apply result", Error: err.Error()})
		return
	}

	now := time.Now()
	inv, err := eng.BuildInventory(now)
	if err != nil {
		s.render(w, "apply_result", applyResultView{Title: "Apply result", Error: err.Error()})
		return
	}

	plan, err := eng.BuildPlan(inv, now)
	if err != nil {
		s.render(w, "apply_result", applyResultView{Title: "Apply result", Error: err.Error()})
		return
	}

	result, err := eng.Movers().Apply(plan, now)
	if err != nil {
		s.render(w, "apply_result", applyResultView{Title: "Apply result", Error: err.Error()})
		return
	}

	view := applyResultView{Title: "Apply result", Moved: len(result.Moved)}
	for _, f := range result.Failed {
		view.Failed = append(view.Failed, failedMoveView{Title: f.Entry.Item.Title, Err: f.Err.Error()})
	}

	if len(result.Moved) > 0 {
		if jf := eng.JellyfinClient(); jf != nil {
			if err := jf.RefreshLibrary(); err != nil {
				view.JellyfinError = err.Error()
			} else {
				view.JellyfinRefreshed = true
			}
		}
	}

	s.render(w, "apply_result", view)
}
