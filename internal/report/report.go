// Package report renders human-readable tables for tier usage, scored
// inventory, and move plans. It never makes decisions - purely formatting.
package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/engine"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/planner"
	"github.com/vocoder/coldarr/internal/scoring"
)

func newTabwriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
}

func fmtGB(b int64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
}

func fmtGBu(b uint64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
}

// TierUsage prints one row per configured path: role, tier, target/max
// thresholds (cold tiers only - hot storage isn't steered toward a usage
// level), and current usage, or the reason a path is being skipped. A path
// that's the same physical volume as another configured path (however
// differently named) is flagged - Coldarr treats their capacity as
// shared, and moving into one affects how much room the other has.
func TierUsage(w io.Writer, inv *engine.Inventory) {
	tw := newTabwriter(w)
	fmt.Fprintln(tw, "TIER\tROLE\tPATH\tUSED\tFREE\tTOTAL\tUSED%\tTARGET%\tMAX%\tSTATUS")

	// Stable order: tiers as configured, paths as configured.
	for _, tier := range inv.Tiers {
		targetCol, maxCol := "-", "-"
		if tier.Role == model.RoleCold {
			targetCol = fmt.Sprintf("%.0f", tier.TargetUsedPercent)
			maxCol = fmt.Sprintf("%.0f", tier.MaxUsedPercent)
		}
		for _, path := range tier.Paths {
			status := inv.PathStatus[path]
			if status.Err != nil {
				fmt.Fprintf(tw, "%s\t%s\t%s\t-\t-\t-\t-\t%s\t%s\tUNAVAILABLE: %v\n",
					tier.Name, tier.Role, path, targetCol, maxCol, status.Err)
				continue
			}
			u := status.Usage
			statusCol := "ok"
			if siblings := inv.SharedVolumePaths(path); len(siblings) > 0 {
				statusCol = fmt.Sprintf("ok (shares a disk with %s)", strings.Join(siblings, ", "))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.1f\t%s\t%s\t%s\n",
				tier.Name, tier.Role, path, fmtGBu(u.UsedBytes), fmtGBu(u.FreeBytes), fmtGBu(u.TotalBytes),
				u.UsedPercent, targetCol, maxCol, statusCol)
		}
	}
	tw.Flush()
}

// Warnings prints non-fatal problems encountered while building the
// inventory (e.g. Jellyfin favorites couldn't be fetched).
func Warnings(w io.Writer, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintln(w, "\nWarnings:")
	for _, warn := range warnings {
		fmt.Fprintf(w, "  - %s\n", warn)
	}
}

// Summary prints decision counts plus the top cold candidates and any
// items that look misplaced relative to their current tier's role.
func Summary(w io.Writer, inv *engine.Inventory, topN int) {
	var protected, hot, cold int
	var hotOnCold, coldOnHot []planner.ItemEval

	for _, ie := range inv.Items {
		switch ie.Eval.Decision {
		case scoring.Protected:
			protected++
		case scoring.Hot:
			hot++
		case scoring.Cold:
			cold++
		}

		tier, ok := inv.TierOf(ie.Item.RootFolderPath)
		if !ok {
			continue
		}
		if tier.Role == model.RoleCold && ie.Eval.Decision == scoring.Hot {
			hotOnCold = append(hotOnCold, ie)
		}
		if tier.Role == model.RoleHot && ie.Eval.Decision == scoring.Cold {
			coldOnHot = append(coldOnHot, ie)
		}
	}

	fmt.Fprintf(w, "\nInventory: %d items (%d protected, %d hot, %d cold candidates)\n",
		len(inv.Items), protected, hot, cold)

	sort.Slice(coldOnHot, func(i, j int) bool { return coldOnHot[i].Eval.Score > coldOnHot[j].Eval.Score })
	fmt.Fprintf(w, "\nTop cold candidates currently on hot storage (move first, cold destination room permitting):\n")
	tw := newTabwriter(w)
	fmt.Fprintln(tw, "TITLE\tTYPE\tSIZE\tSCORE\tWHY")
	for i, ie := range coldOnHot {
		if i >= topN {
			break
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f\t%s\n", ie.Item.Title, ie.Item.Type, fmtGB(ie.Item.SizeBytes), ie.Eval.Score, firstReason(ie.Eval.Reasons))
	}
	tw.Flush()
	if len(coldOnHot) == 0 {
		fmt.Fprintln(w, "  (none)")
	}

	if len(hotOnCold) > 0 {
		fmt.Fprintf(w, "\nItems that look misplaced (scored hot but currently on cold storage):\n")
		tw := newTabwriter(w)
		fmt.Fprintln(tw, "TITLE\tTYPE\tSIZE\tCURRENT PATH\tWHY")
		for _, ie := range hotOnCold {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ie.Item.Title, ie.Item.Type, fmtGB(ie.Item.SizeBytes), ie.Item.RootFolderPath, firstReason(ie.Eval.Reasons))
		}
		tw.Flush()
	}
}

func firstReason(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return reasons[0]
}

// Plan prints the proposed moves and any planner warnings. It makes no
// changes - callers decide separately whether to execute it.
func Plan(w io.Writer, plan *planner.Plan) {
	if len(plan.Entries) == 0 {
		fmt.Fprintln(w, "\nNo moves needed - no cold-eligible items found on hot storage with room to accept them.")
	} else {
		var total int64
		tw := newTabwriter(w)
		fmt.Fprintln(tw, "TITLE\tTYPE\tSIZE\tFROM\tTO\tSCORE\tWHY")
		for _, e := range plan.Entries {
			total += e.Item.SizeBytes
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s (%s)\t%s (%s)\t%.1f\t%s\n",
				e.Item.Title, e.Item.Type, fmtGB(e.Item.SizeBytes), e.FromTier, e.FromPath, e.ToTier, e.ToPath, e.Score, firstReason(e.Reasons))
		}
		tw.Flush()
		fmt.Fprintf(w, "\n%d items, %s total\n", len(plan.Entries), fmtGB(total))
	}

	if len(plan.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, warn := range plan.Warnings {
			fmt.Fprintf(w, "  - %s\n", warn)
		}
	}
}

// ProjectedUsage prints before/after usage for every path touched by the
// plan (or all paths, if includeAll is true).
func ProjectedUsage(w io.Writer, before map[string]diskusage.Usage, after map[string]diskusage.Usage, tiers []model.Tier) {
	fmt.Fprintln(w, "\nProjected usage after plan:")
	tw := newTabwriter(w)
	fmt.Fprintln(tw, "TIER\tPATH\tBEFORE\tAFTER")
	for _, tier := range tiers {
		for _, path := range tier.Paths {
			b, ok := before[path]
			if !ok {
				continue
			}
			a := after[path]
			if fmt.Sprintf("%.1f", b.UsedPercent) == fmt.Sprintf("%.1f", a.UsedPercent) {
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%.1f%%\t%.1f%%\n", tier.Name, path, b.UsedPercent, a.UsedPercent)
		}
	}
	tw.Flush()
}
