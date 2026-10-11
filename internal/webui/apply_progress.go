package webui

import (
	"fmt"
	"time"

	"github.com/vocoder/coldarr/internal/mover"
)

type applyProgressView struct {
	Percent   int
	Elapsed   string
	Remaining string
	Hint      string
	TotalSize string
	Total     int
	Resolved  int
}

func buildApplyProgress[E mover.Entry](snap mover.RunSnapshot[E], now time.Time) applyProgressView {
	estimate := snap.Estimate(now)
	view := applyProgressView{
		Percent: estimate.Percent,
		Elapsed: formatMoveDuration(estimate.Elapsed),
		Total:   len(snap.Entries),
	}
	var totalBytes float64
	for _, e := range snap.Entries {
		totalBytes += float64(max(0, e.Entry.MoveSize()))
		if e.Status == mover.StatusDone || e.Status == mover.StatusFailed {
			view.Resolved++
		}
	}
	if totalBytes > 0 {
		view.TotalSize = fmt.Sprintf("%.1f GB", totalBytes/(1<<30))
	}
	switch {
	case snap.Done:
		view.Remaining = "Moves finished"
	case estimate.Overdue:
		view.Remaining = "Re-estimating…"
		view.Hint = "A current move is taking longer than the observed pace. The estimate will update when it finishes."
	case estimate.Known:
		view.Remaining = "About " + formatMoveDuration(estimate.Remaining) + " left"
		view.Hint = "Based on move sizes and observed pace per drive. Final library updates may take extra time."
	default:
		view.Remaining = "Estimating…"
		view.Hint = "Waiting for the first completed move to learn the transfer pace."
	}
	return view
}

// Round up to minutes: a seconds-precise countdown implies more certainty than
// whole-item timings can provide, especially while a large transfer is running.
func formatMoveDuration(d time.Duration) string {
	if d < time.Minute {
		return "< 1m"
	}
	minutes := int64(d / time.Minute)
	if d%time.Minute != 0 {
		minutes++
	}
	if minutes < 60 {
		return fmt.Sprintf("%dm", minutes)
	}
	if minutes < 24*60 {
		return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
	}
	hours := (minutes + 59) / 60
	return fmt.Sprintf("%dd %dh", hours/24, hours%24)
}
