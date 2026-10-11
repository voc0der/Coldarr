package webui

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/vocoder/coldarr/internal/config"
)

// labelRowView is one row of the Labels table: a category or tag, and the
// rule (if any) saved for it.
type labelRowView struct {
	Index int
	Label string
	// Where says how qBittorrent uses the label right now - "category",
	// "tag", or both - and is empty for a saved rule whose label
	// qBittorrent no longer has. Such rules are kept until someone removes
	// them: a category can disappear and come back, and nobody should have
	// to set its rule up again when it does.
	Where       string
	Action      string
	ImportedBy  string
	IsSeedLabel bool
}

type importAppView struct {
	Value string
	Label string
}

type downloadsSettingsData struct {
	Title string
	Error string
	Saved string

	SettleDays            int
	SeedLabel             string
	AllowDisablingAutoTMM bool
	Categories            []string

	// LabelsError says why qBittorrent's labels couldn't be read; the
	// saved rules still show, and can still be edited.
	LabelsError string
	Labels      []labelRowView
	ImportApps  []importAppView
}

func (s *Server) downloadsSettingsData() downloadsSettingsData {
	cfg := s.currentConfig().Torrents
	data := downloadsSettingsData{
		Title:                 "Downloads",
		SettleDays:            cfg.SettleDays,
		SeedLabel:             cfg.SeedLabel,
		AllowDisablingAutoTMM: cfg.AllowDisablingAutoTMM,
		ImportApps: []importAppView{
			{Value: "radarr", Label: "Radarr"},
			{Value: "sonarr", Label: "Sonarr"},
			{Value: "lidarr", Label: "Lidarr"},
		},
	}

	where := map[string][]string{}
	eng, err := s.newEngine()
	switch {
	case err != nil:
		data.LabelsError = err.Error()
	case eng.Qbit == nil:
		data.LabelsError = "qBittorrent isn't connected, so only saved rules are shown."
	default:
		categories, catErr := eng.Qbit.Categories()
		tags, tagErr := eng.Qbit.Tags()
		if catErr != nil || tagErr != nil {
			data.LabelsError = fmt.Sprintf("Couldn't read qBittorrent's labels, so only saved rules are shown: %v", firstErr(catErr, tagErr))
			break
		}
		for name := range categories {
			where[name] = append(where[name], "category")
			data.Categories = append(data.Categories, name)
		}
		for _, tag := range tags {
			where[tag] = append(where[tag], "tag")
		}
		sort.Strings(data.Categories)
	}

	// Every label qBittorrent has, then every saved rule it doesn't - each
	// group alphabetical, so the table reads the same from one visit to
	// the next however the labels come back.
	var present, missing []string
	for name := range where {
		present = append(present, name)
	}
	for _, r := range cfg.Labels {
		if _, ok := where[r.Label]; !ok {
			missing = append(missing, r.Label)
		}
	}
	sort.Strings(present)
	sort.Strings(missing)
	for i, name := range append(present, missing...) {
		row := labelRowView{Index: i, Label: name, Where: strings.Join(where[name], " and "), IsSeedLabel: name == cfg.SeedLabel}
		if r, ok := cfg.Rule(name); ok {
			row.Action, row.ImportedBy = string(r.Action), r.ImportedBy
		}
		data.Labels = append(data.Labels, row)
	}
	return data
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleDownloadsSettingsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings_downloads", s.downloadsSettingsData())
}

func (s *Server) renderDownloadsSettings(w http.ResponseWriter, saved string, err error) {
	data := s.downloadsSettingsData()
	if err != nil {
		data.Error = err.Error()
	} else {
		data.Saved = saved
	}
	s.render(w, "settings_downloads", data)
}

func (s *Server) handleDownloadsMovingSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	settle, err := strconv.Atoi(strings.TrimSpace(r.FormValue("settle_days")))
	if err != nil || settle < 1 {
		s.renderDownloadsSettings(w, "", fmt.Errorf("settle time must be a whole number of days, at least 1"))
		return
	}
	seedLabel := strings.TrimSpace(r.FormValue("seed_label"))
	if seedLabel == "" {
		s.renderDownloadsSettings(w, "", fmt.Errorf("the seed label can't be empty"))
		return
	}

	err = s.updateTorrents(func(t *config.TorrentsConfig) {
		t.SettleDays = settle
		t.SeedLabel = seedLabel
		t.AllowDisablingAutoTMM = r.FormValue("allow_disabling_auto_tmm") == "on"
	})
	s.renderDownloadsSettings(w, "Moving settings saved.", err)
}

// handleDownloadsLabelsSave replaces the label rules with the table as
// submitted. A row left at "No rule" is dropped - that label is left
// alone, the same as any label nobody has set up - and "Remove" drops a
// saved rule qBittorrent no longer has. Rows arrive as label_N/action_N/
// imported_by_N, numbered rather than keyed by label, so a label with odd
// characters in it can't collide with the form's own field names.
func (s *Server) handleDownloadsLabelsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	var rules []config.LabelRule
	for i := 0; ; i++ {
		label, ok := r.Form[fmt.Sprintf("label_%d", i)]
		if !ok {
			break
		}
		action := config.LabelAction(r.FormValue(fmt.Sprintf("action_%d", i)))
		if action == "" || r.FormValue(fmt.Sprintf("remove_%d", i)) == "on" {
			continue
		}
		rule := config.LabelRule{Label: strings.TrimSpace(label[0]), Action: action}
		if action == config.LabelMove {
			rule.ImportedBy = r.FormValue(fmt.Sprintf("imported_by_%d", i))
		}
		rules = append(rules, rule)
	}

	err := s.updateTorrents(func(t *config.TorrentsConfig) {
		t.Labels = rules
	})
	s.renderDownloadsSettings(w, "Label rules saved.", err)
}
