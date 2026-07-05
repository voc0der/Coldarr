package webui

import (
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
	FreeBytes        int64
	TotalBytes       int64
	UsedPercent      float64
	HasThresholds    bool // false for hot tiers - not steered toward a usage level
	TargetPercent    float64
	MaxPercent       float64
	StatusMsg        string
	StatusClass      string
	SharesVolumeWith []string
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
				row.UsedBytes = int64(status.Usage.UsedBytes)
				row.FreeBytes = int64(status.Usage.FreeBytes)
				row.TotalBytes = int64(status.Usage.TotalBytes)
				row.UsedPercent = status.Usage.UsedPercent
				row.StatusMsg = "ok"
				row.StatusClass = "ok"
				row.SharesVolumeWith = inv.SharedVolumePaths(path)
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
