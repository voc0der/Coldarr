package webui

import (
	"math"
	"net/http"
	"time"

	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/scoring"
)

type tierRow struct {
	TierName         string
	Role             model.TierRole
	Path             string
	Available        bool
	UsedBytes        int64
	TotalBytes       int64
	UsedPercent      float64
	UsedPercentOver  bool // usage visibly past the tier's ceiling (MaxPercent for cold, 100% for hot) at the 1-decimal precision the UI displays - never flagged for sub-decimal noise right at the ceiling, since cold tiers are designed to pack up to it
	HasThresholds    bool // false for hot tiers - not steered toward a usage level
	TargetPercent    float64
	MaxPercent       float64
	StatusMsg        string
	StatusClass      string
	SharesVolumeWith []string
	SharedColorClass string // CSS class coloring the Tier column's link glyph; empty unless SharesVolumeWith is non-empty
}

// sharedVolumeColorClasses is the fixed color wheel for the Tier column's
// link glyph: tiers sharing a physical disk get the same color, assigned in
// the order their shared group is first seen while walking the (statically
// ordered) config - so the same config always paints the same colors, never
// randomly. Skips the hues --ok/--danger already claim elsewhere in this UI
// (green/red), so a link color is never mistaken for a status.
var sharedVolumeColorClasses = []string{
	"tier-link-1", // blue
	"tier-link-2", // aqua
	"tier-link-3", // yellow
	"tier-link-4", // violet
	"tier-link-5", // magenta
	"tier-link-6", // orange
}

type dashboardData struct {
	Title              string
	Error              string
	Warnings           []string
	RadarrConfigured   bool
	SonarrConfigured   bool
	JellyfinConfigured bool
	Rows               []tierRow
	TotalItems         int
	ProtectedCount     int
	HotCount           int
	ColdCount          int
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := dashboardData{Title: "Dashboard"}

	eng, err := s.newEngine()
	if err != nil {
		data.Error = err.Error()
		s.render(w, "dashboard", data)
		return
	}

	data.RadarrConfigured = eng.Radarr != nil
	data.SonarrConfigured = eng.Sonarr != nil
	data.JellyfinConfigured = eng.JellyfinClient() != nil

	inv, err := eng.BuildInventory(time.Now())
	if err != nil {
		data.Error = err.Error()
		s.render(w, "dashboard", data)
		return
	}

	volumeOf := inv.VolumeOf()
	deviceColorIdx := make(map[uint64]int)
	nextColor := 0

	for _, tier := range inv.Tiers {
		for _, path := range tier.Paths {
			status := inv.PathStatus[path]
			row := tierRow{
				TierName:      tier.Name,
				Role:          tier.Role,
				Path:          path,
				HasThresholds: tier.Role == model.RoleCold,
				TargetPercent: tier.TargetUsedPercent,
				MaxPercent:    tier.MaxUsedPercent,
			}
			if status.Err != nil {
				row.StatusMsg = status.Err.Error()
				row.StatusClass = "danger"
			} else {
				row.Available = true
				row.UsedBytes = int64(status.Usage.UsedBytes)   //nolint:gosec // disk usage byte counts never approach int64 overflow range
				row.TotalBytes = int64(status.Usage.TotalBytes) //nolint:gosec // disk usage byte counts never approach int64 overflow range
				row.UsedPercent = status.Usage.UsedPercent
				row.StatusMsg = "ok"
				row.StatusClass = "ok"
				row.SharesVolumeWith = inv.SharedVolumePaths(path)
				if len(row.SharesVolumeWith) > 0 {
					dev := volumeOf[path]
					idx, seen := deviceColorIdx[dev]
					if !seen {
						idx = nextColor % len(sharedVolumeColorClasses)
						deviceColorIdx[dev] = idx
						nextColor++
					}
					row.SharedColorClass = sharedVolumeColorClasses[idx]
				}
				usedRounded := roundTenth(row.UsedPercent)
				if row.HasThresholds {
					row.UsedPercentOver = usedRounded > roundTenth(row.MaxPercent)
				} else {
					row.UsedPercentOver = usedRounded >= 100
				}
			}
			data.Rows = append(data.Rows, row)
		}
	}

	data.Warnings = inv.Warnings
	data.TotalItems = len(inv.Items)
	for _, it := range inv.Items {
		switch it.Eval.Decision {
		case scoring.Protected:
			data.ProtectedCount++
		case scoring.Hot:
			data.HotCount++
		case scoring.Cold:
			data.ColdCount++
		}
	}

	s.render(w, "dashboard", data)
}

// roundTenth matches the precision pct() actually displays (one decimal
// place), so the danger threshold only trips on a difference a viewer can
// see - not sub-decimal jitter from the underlying block-count math that
// would otherwise flag a tier sitting exactly at its configured ceiling
// (its designed steady state, not a fault) as if it were overflowing.
func roundTenth(f float64) float64 {
	return math.Round(f*10) / 10
}
