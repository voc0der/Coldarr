package webui

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/engine"
	"github.com/vocoder/coldarr/internal/trackercache"
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

// trackerOverrideView is one row of the tracker overrides table.
type trackerOverrideView struct {
	Host string
	Days int
	// Downloads is how many scanned downloads use this tracker right now.
	// A saved override with none is kept - downloads come and go - and
	// flagged, so it can be removed if it's really no longer wanted.
	Downloads int
}

type downloadsSettingsData struct {
	Title string
	Error string
	Saved string

	DefaultSeedDays int
	GraceDays       int
	Overrides       []trackerOverrideView
	// TrackerHosts feeds the override picker: every tracker host seen by
	// a scan, plus each download's current tracker.
	TrackerHosts    []string
	TrackersScanned string
	// TrackerError says why the live trackers couldn't be read.
	TrackerError string

	SettleDays            int
	SeedLabel             string
	AllowDisablingAutoTMM bool
	Categories            []string

	RemovalMode string
	MaxPerRun   int

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
		DefaultSeedDays:       cfg.Seeding.MinSeedDays,
		GraceDays:             cfg.Seeding.Grace(),
		SettleDays:            cfg.SettleDays,
		RemovalMode:           string(cfg.Removal.EffectiveMode()),
		MaxPerRun:             cfg.Removal.Limit(),
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
	if err != nil {
		eng = nil
	}
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

	s.addTrackerData(&data, eng, cfg.Seeding)

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

// addTrackerData fills in the Seeding rules card: the overrides with how
// many downloads use each, and the picker's hosts.
func (s *Server) addTrackerData(data *downloadsSettingsData, eng *engine.Engine, seeding config.SeedingConfig) {
	hosts := map[string]bool{}
	var snap trackercache.Snapshot
	if eng != nil {
		snap = eng.Trackers.Get()
		for _, h := range snap.SeenHosts() {
			hosts[h] = true
		}
		if eng.Qbit != nil {
			if list, err := eng.Qbit.Torrents(); err != nil {
				data.TrackerError = fmt.Sprintf("Couldn't read qBittorrent's current trackers: %v", err)
			} else {
				for _, t := range list {
					if t.TrackerHost != "" {
						hosts[t.TrackerHost] = true
					}
				}
			}
		}
	}
	if !snap.RefreshedAt.IsZero() {
		data.TrackersScanned = snap.RefreshedAt.Format("2006-01-02 15:04")
	}

	for _, o := range seeding.Overrides {
		row := trackerOverrideView{Host: o.Host, Days: o.MinSeedDays}
		for _, e := range snap.Downloads {
			for _, h := range e.Hosts {
				if h == o.Host || strings.HasSuffix(h, "."+o.Host) {
					row.Downloads++
					break
				}
			}
		}
		data.Overrides = append(data.Overrides, row)
		hosts[o.Host] = true
	}
	sort.Slice(data.Overrides, func(i, j int) bool { return data.Overrides[i].Host < data.Overrides[j].Host })
	for h := range hosts {
		data.TrackerHosts = append(data.TrackerHosts, h)
	}
	sort.Strings(data.TrackerHosts)
}

// handleDownloadsSeedingSave saves the default requirement and the grace
// margin.
func (s *Server) handleDownloadsSeedingSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	days, err1 := strconv.Atoi(strings.TrimSpace(r.FormValue("min_seed_days")))
	grace, err2 := strconv.Atoi(strings.TrimSpace(r.FormValue("grace_days")))
	if err1 != nil || err2 != nil || days < 0 || grace < 0 {
		s.renderDownloadsSettings(w, "", fmt.Errorf("seeding days and grace days must be whole numbers, 0 or more"))
		return
	}
	err := s.updateTorrents(func(t *config.TorrentsConfig) {
		t.Seeding.MinSeedDays = days
		t.Seeding.GraceDays = &grace
	})
	s.renderDownloadsSettings(w, "Seeding defaults saved.", err)
}

// handleDownloadsTrackerSave adds a tracker override, or changes the one
// already saved for that host. Whatever was typed or pasted - an announce
// URL included - is reduced to its host first, so a passkey never reaches
// coldarr.yaml.
func (s *Server) handleDownloadsTrackerSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	host := config.NormalizeTrackerHost(r.FormValue("host"))
	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("min_seed_days")))
	if host == "" || err != nil || days < 0 {
		s.renderDownloadsSettings(w, "", fmt.Errorf("an override needs a tracker host and a whole number of days, 0 or more"))
		return
	}
	err = s.updateTorrents(func(t *config.TorrentsConfig) {
		t.Seeding.Overrides = append([]config.TrackerOverride(nil), t.Seeding.Overrides...)
		for i := range t.Seeding.Overrides {
			if t.Seeding.Overrides[i].Host == host {
				t.Seeding.Overrides[i].MinSeedDays = days
				return
			}
		}
		t.Seeding.Overrides = append(t.Seeding.Overrides, config.TrackerOverride{Host: host, MinSeedDays: days})
	})
	s.renderDownloadsSettings(w, fmt.Sprintf("Override for %s saved.", host), err)
}

func (s *Server) handleDownloadsTrackerDelete(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	host := r.FormValue("host")
	err := s.updateTorrents(func(t *config.TorrentsConfig) {
		kept := make([]config.TrackerOverride, 0, len(t.Seeding.Overrides))
		for _, o := range t.Seeding.Overrides {
			if o.Host != host {
				kept = append(kept, o)
			}
		}
		t.Seeding.Overrides = kept
	})
	s.renderDownloadsSettings(w, fmt.Sprintf("Override for %s removed.", host), err)
}

// handleDownloadsRemovalSave saves the removal mode and the per-run cap.
func (s *Server) handleDownloadsRemovalSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	mode := config.RemovalMode(r.FormValue("mode"))
	limit, err := strconv.Atoi(strings.TrimSpace(r.FormValue("max_per_run")))
	if err != nil || limit < 1 {
		s.renderDownloadsSettings(w, "", fmt.Errorf("the most to remove per run must be a whole number, at least 1"))
		return
	}
	err = s.updateTorrents(func(t *config.TorrentsConfig) {
		t.Removal.Mode = mode
		t.Removal.MaxPerRun = limit
	})
	s.renderDownloadsSettings(w, "Removal settings saved.", err)
}

// handleDownloadsTrackerScan is the Seeding rules card's own "Scan now" -
// the same scan as the scheduled task's (see scanTrackersNow).
func (s *Server) handleDownloadsTrackerScan(w http.ResponseWriter, r *http.Request) {
	looked, err := s.scanTrackersNow()
	s.renderDownloadsSettings(w, fmt.Sprintf("Tracker scan finished - %d download(s) looked up.", looked), err)
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
