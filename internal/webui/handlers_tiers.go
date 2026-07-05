package webui

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/model"
)

type tierPathStatus struct {
	Path        string
	Available   bool
	UsedPercent float64
	Err         string
}

type tierListRow struct {
	Tier  model.Tier
	Paths []tierPathStatus
}

type tiersData struct {
	Title string
	Error string
	Saved string
	Tiers []tierListRow
}

func (s *Server) tierListRows() []tierListRow {
	cfg := s.currentConfig()
	rows := make([]tierListRow, 0, len(cfg.Tiers))
	for _, t := range cfg.Tiers {
		row := tierListRow{Tier: t}
		for _, p := range t.Paths {
			ps := tierPathStatus{Path: p}
			if err := diskusage.CheckPath(p, t.RequireMount); err != nil {
				ps.Err = err.Error()
			} else if u, err := diskusage.Stat(p); err != nil {
				ps.Err = err.Error()
			} else {
				ps.Available = true
				ps.UsedPercent = u.UsedPercent
			}
			row.Paths = append(row.Paths, ps)
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) handleTiersPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "tiers", tiersData{Title: "Tiers", Tiers: s.tierListRows()})
}

type tierFormData struct {
	Title     string
	Error     string
	Editing   bool
	OrigName  string
	Tier      model.Tier
	PathsText string
	HasMovie  bool
	HasTV     bool
}

func (s *Server) handleTierNewForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, "tier_form", tierFormData{Title: "Add tier"})
}

func (s *Server) handleTierEditForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := s.currentConfig()
	for _, t := range cfg.Tiers {
		if t.Name == name {
			s.render(w, "tier_form", tierFormData{
				Title:     "Edit tier",
				Editing:   true,
				OrigName:  t.Name,
				Tier:      t,
				PathsText: strings.Join(t.Paths, "\n"),
				HasMovie:  t.AcceptsMediaType(model.Movie),
				HasTV:     t.AcceptsMediaType(model.TV),
			})
			return
		}
	}
	http.NotFound(w, r)
}

func tierFromForm(r *http.Request) (model.Tier, error) {
	_ = r.ParseForm()

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return model.Tier{}, fmt.Errorf("name is required")
	}

	role := model.TierRole(r.FormValue("role"))

	var paths []string
	for _, line := range strings.Split(r.FormValue("paths"), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}

	var media []model.MediaType
	if r.FormValue("media_movie") == "on" {
		media = append(media, model.Movie)
	}
	if r.FormValue("media_tv") == "on" {
		media = append(media, model.TV)
	}

	// target/max only apply to cold tiers; the hot-role form leaves these
	// fields hidden and blank, so an empty value just means "not set"
	// rather than a parse error. A genuinely malformed cold-tier value
	// still gets caught by config.ValidateTiers's range check below.
	target := parseFloatOrZero(r.FormValue("target_used_percent"))
	max := parseFloatOrZero(r.FormValue("max_used_percent"))

	return model.Tier{
		Name:              name,
		Role:              role,
		Paths:             paths,
		Media:             media,
		TargetUsedPercent: target,
		MaxUsedPercent:    max,
		RequireMount:      r.FormValue("require_mount") == "on",
	}, nil
}

func parseFloatOrZero(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func tierFormFromRequest(title string, editing bool, origName string, r *http.Request, tier model.Tier, errMsg string) tierFormData {
	return tierFormData{
		Title:     title,
		Error:     errMsg,
		Editing:   editing,
		OrigName:  origName,
		Tier:      tier,
		PathsText: r.FormValue("paths"),
		HasMovie:  r.FormValue("media_movie") == "on",
		HasTV:     r.FormValue("media_tv") == "on",
	}
}

func (s *Server) handleTierCreate(w http.ResponseWriter, r *http.Request) {
	tier, err := tierFromForm(r)
	if err != nil {
		s.render(w, "tier_form", tierFormFromRequest("Add tier", false, "", r, tier, err.Error()))
		return
	}

	err = s.updateTiers(func(tiers []model.Tier) ([]model.Tier, error) {
		for _, t := range tiers {
			if t.Name == tier.Name {
				return nil, fmt.Errorf("a tier named %q already exists", tier.Name)
			}
		}
		return append(tiers, tier), nil
	})
	if err != nil {
		s.render(w, "tier_form", tierFormFromRequest("Add tier", false, "", r, tier, err.Error()))
		return
	}

	http.Redirect(w, r, "/tiers", http.StatusSeeOther)
}

func (s *Server) handleTierUpdate(w http.ResponseWriter, r *http.Request) {
	origName := r.PathValue("name")

	tier, err := tierFromForm(r)
	if err != nil {
		s.render(w, "tier_form", tierFormFromRequest("Edit tier", true, origName, r, tier, err.Error()))
		return
	}

	err = s.updateTiers(func(tiers []model.Tier) ([]model.Tier, error) {
		out := make([]model.Tier, 0, len(tiers))
		found := false
		for _, t := range tiers {
			if t.Name == origName {
				out = append(out, tier)
				found = true
				continue
			}
			if t.Name == tier.Name {
				return nil, fmt.Errorf("a tier named %q already exists", tier.Name)
			}
			out = append(out, t)
		}
		if !found {
			return nil, fmt.Errorf("tier %q not found", origName)
		}
		return out, nil
	})
	if err != nil {
		s.render(w, "tier_form", tierFormFromRequest("Edit tier", true, origName, r, tier, err.Error()))
		return
	}

	http.Redirect(w, r, "/tiers", http.StatusSeeOther)
}

func (s *Server) handleTierDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	err := s.updateTiers(func(tiers []model.Tier) ([]model.Tier, error) {
		out := make([]model.Tier, 0, len(tiers))
		for _, t := range tiers {
			if t.Name != name {
				out = append(out, t)
			}
		}
		return out, nil
	})
	if err != nil {
		s.render(w, "tiers", tiersData{Title: "Tiers", Error: err.Error(), Tiers: s.tierListRows()})
		return
	}

	http.Redirect(w, r, "/tiers", http.StatusSeeOther)
}
