package webui

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/scoring"
	"github.com/vocoder/coldarr/internal/torrents"
)

type tierRow struct {
	TierName        string
	Role            model.TierRole
	IsDownloads     bool
	Path            string
	Available       bool
	UsedBytes       int64
	TotalBytes      int64
	UsedPercent     float64
	UsedPercentOver bool // cold usage visibly past its hard ceiling, or hot literally full, at the 1-decimal precision the UI displays
	HasTarget       bool // false for hot tiers, which have a reclaim max but no fill target
	TargetPercent   float64
	MaxPercent      float64
	UsesDefaultMax  bool
	StatusMsg       string
	StatusClass     string
	// Wedged is seed storage that downloads Coldarr doesn't manage fill
	// past its max on their own (see engine.DownloadsInventory.Wedged) -
	// set to a description of why, which no amount of waiting will change.
	Wedged           string
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
	// Lidarr and qBittorrent are optional extras for download tiers, so
	// unlike the three above they're only listed once configured.
	LidarrConfigured      bool
	QbittorrentConfigured bool
	Rows                  []tierRow
	TotalItems            int
	ProtectedCount        int
	HotCount              int
	ColdCount             int
	// Downloads summarizes download tiering, when it's set up: nil
	// otherwise, so a library-only dashboard looks as it always has.
	Downloads *downloadCounts
}

// downloadCounts is the dashboard's download-tiering summary - how many of
// the downloads Coldarr manages are in each state.
type downloadCounts struct {
	Active  int
	Waiting int
	Ready   int
	Held    int
	Parked  int
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
	data.LidarrConfigured = eng.Lidarr != nil
	data.QbittorrentConfigured = eng.Qbit != nil

	inv, err := eng.BuildInventory(time.Now())
	if err != nil {
		data.Error = err.Error()
		s.render(w, "dashboard", data)
		return
	}

	// Download tiering is summarized from what's already known - no
	// import checks are made just to draw the dashboard (see
	// BuildDownloadsInventory). A failure is a warning, not an error: the
	// rest of the dashboard is still worth showing.
	wedged := map[string]int64{}
	if eng.DownloadsEnabled() {
		if dl, err := eng.BuildDownloadsInventory(time.Now(), false); err != nil {
			data.Warnings = append(data.Warnings, "Couldn't read downloads: "+err.Error())
		} else {
			counts := dl.Count()
			data.Downloads = &downloadCounts{
				Active:  counts[torrents.Active],
				Waiting: counts[torrents.Waiting],
				Ready:   counts[torrents.Ready],
				Held:    counts[torrents.Held],
				Parked:  counts[torrents.Parked],
			}
			data.Warnings = append(data.Warnings, dl.Warnings...)
			wedged = dl.Wedged()
		}
	}

	volumeOf := inv.VolumeOf()
	deviceColorIdx := make(map[uint64]int)
	nextColor := 0

	for _, tier := range inv.Tiers {
		for _, path := range tier.Paths {
			status := inv.PathStatus[path]
			row := dashboardTierRow(tier, path)
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
				row.UsedPercentOver = usagePastHardLimit(row)
				if n, ok := wedged[path]; ok {
					row.Wedged = fmt.Sprintf("%.1f GB here isn't Coldarr's to move - downloads it leaves alone, or other files - and that alone fills it past its %.1f%% max, so nothing can move here until some of it is cleared or the max is raised.", float64(n)/(1<<30), row.MaxPercent)
				}
			}
			data.Rows = append(data.Rows, row)
		}
	}

	data.Warnings = append(inv.Warnings, data.Warnings...)
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

func dashboardTierRow(tier model.Tier, path string) tierRow {
	return tierRow{
		TierName:       tier.Name,
		Role:           tier.Role,
		IsDownloads:    tier.IsDownloads(),
		Path:           path,
		HasTarget:      tier.Role == model.RoleCold,
		TargetPercent:  tier.TargetUsedPercent,
		MaxPercent:     tier.EffectiveMaxUsedPercent(),
		UsesDefaultMax: tier.Role == model.RoleHot && tier.MaxUsedPercent == 0,
	}
}

func usagePastHardLimit(row tierRow) bool {
	usedRounded := roundTenth(row.UsedPercent)
	if row.HasTarget {
		return usedRounded > roundTenth(row.MaxPercent)
	}
	// Hot max is only an admission limit for future reclaims; runoff hot
	// storage is allowed to already sit above it.
	return usedRounded >= 100
}

// roundTenth matches the precision pct() actually displays (one decimal
// place), so the danger threshold only trips on a difference a viewer can
// see - not sub-decimal jitter from the underlying block-count math that
// would otherwise flag a tier sitting exactly at its configured ceiling
// (its designed steady state, not a fault) as if it were overflowing.
func roundTenth(f float64) float64 {
	return math.Round(f*10) / 10
}
