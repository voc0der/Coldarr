package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/scheduler"
)

func TestApplyDefaults_SchedulerStaysDisabled(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)

	for name, s := range map[string]scheduler.Schedule{
		"run_plan":      cfg.Scheduler.RunPlan,
		"rescan_cold":   cfg.Scheduler.RescanCold,
		"refresh_links": cfg.Scheduler.RefreshLinks,
	} {
		if s.Enabled {
			t.Errorf("%s: Enabled = true after applyDefaults, want false", name)
		}
		if s.Unit != scheduler.Daily {
			t.Errorf("%s: Unit = %q, want %q", name, s.Unit, scheduler.Daily)
		}
		if s.Every != 1 {
			t.Errorf("%s: Every = %d, want 1", name, s.Every)
		}
		if s.At == "" {
			t.Errorf("%s: At is empty, want a default HH:MM", name)
		}
	}

	if cfg.Scheduler.RunPlan.At == cfg.Scheduler.RescanCold.At {
		t.Errorf("run_plan and rescan_cold defaulted to the same time %q - they should be staggered", cfg.Scheduler.RunPlan.At)
	}
	if cfg.Scheduler.RefreshLinks.At == cfg.Scheduler.RunPlan.At || cfg.Scheduler.RefreshLinks.At == cfg.Scheduler.RescanCold.At {
		t.Errorf("refresh_links defaulted to the same time as another task (%q) - they should be staggered", cfg.Scheduler.RefreshLinks.At)
	}
}

func TestApplyDefaults_PreservesExplicitSchedule(t *testing.T) {
	cfg := &Config{Scheduler: SchedulerConfig{
		RunPlan: scheduler.Schedule{Enabled: true, Unit: scheduler.Hourly, Every: 6},
	}}
	applyDefaults(cfg)

	if !cfg.Scheduler.RunPlan.Enabled || cfg.Scheduler.RunPlan.Unit != scheduler.Hourly || cfg.Scheduler.RunPlan.Every != 6 {
		t.Fatalf("applyDefaults overwrote an explicitly configured schedule: %+v", cfg.Scheduler.RunPlan)
	}
}

func TestApplyDefaults_MinMoveSizeGB(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)

	if cfg.Policy.MinMoveSizeGB != 1 {
		t.Fatalf("MinMoveSizeGB = %v, want 1 (a stray near-zero-size item must not slip through the planner's size filter unfiltered)", cfg.Policy.MinMoveSizeGB)
	}

	cfg2 := &Config{Policy: PolicyConfig{MinMoveSizeGB: 5}}
	applyDefaults(cfg2)
	if cfg2.Policy.MinMoveSizeGB != 5 {
		t.Fatalf("applyDefaults overwrote an explicitly configured MinMoveSizeGB: %v", cfg2.Policy.MinMoveSizeGB)
	}
}

func TestApplyDefaults_AuthOIDC(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)

	if cfg.Auth.OIDC.RequiredGroup != "coldarr" {
		t.Fatalf("RequiredGroup = %q, want coldarr", cfg.Auth.OIDC.RequiredGroup)
	}
	if cfg.Auth.OIDC.GroupsClaim != "groups" {
		t.Fatalf("GroupsClaim = %q, want groups", cfg.Auth.OIDC.GroupsClaim)
	}
	if cfg.Auth.OIDC.TokenAuthMethod != "auto" {
		t.Fatalf("TokenAuthMethod = %q, want auto", cfg.Auth.OIDC.TokenAuthMethod)
	}
	if cfg.Auth.OIDC.Enabled {
		t.Fatal("OIDC auth should default to disabled")
	}
}

func TestValidateScheduler(t *testing.T) {
	valid := SchedulerConfig{
		WeeklyOmitDays: []scheduler.Weekday{scheduler.Monday, scheduler.Friday},
		RunPlan:        scheduler.Schedule{Enabled: true, Unit: scheduler.Daily, Every: 1, At: "03:00"},
		RescanCold:     scheduler.Schedule{Enabled: false},
	}
	if err := ValidateScheduler(valid); err != nil {
		t.Fatalf("ValidateScheduler(valid) = %v, want nil", err)
	}

	invalid := SchedulerConfig{
		RunPlan: scheduler.Schedule{Enabled: true, Unit: scheduler.Daily, Every: 0, At: "03:00"},
	}
	if err := ValidateScheduler(invalid); err == nil {
		t.Fatal("ValidateScheduler(invalid) = nil, want an error for run_plan.every=0")
	}

	invalidOmitDay := SchedulerConfig{WeeklyOmitDays: []scheduler.Weekday{"funday"}}
	if err := ValidateScheduler(invalidOmitDay); err == nil {
		t.Fatal("ValidateScheduler(invalidOmitDay) = nil, want an error for an unknown weekly omit day")
	}
}

func TestValidateTiers_HotMaxUsedPercent(t *testing.T) {
	tests := []struct {
		name    string
		max     float64
		wantErr bool
	}{
		{name: "unset uses default", max: 0},
		{name: "positive override", max: 92.5},
		{name: "one hundred allowed", max: 100},
		{name: "negative", max: -1, wantErr: true},
		{name: "over one hundred", max: 100.1, wantErr: true},
		{name: "not a number", max: math.NaN(), wantErr: true},
		{name: "positive infinity", max: math.Inf(1), wantErr: true},
		{name: "negative infinity", max: math.Inf(-1), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tier := model.Tier{
				Name: "hot", Role: model.RoleHot, Paths: []string{"/hot"},
				Media: []model.MediaType{model.Movie}, MaxUsedPercent: tt.max,
			}
			err := ValidateTiers([]model.Tier{tier}, false)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateTiers accepted hot max_used_percent %v", tt.max)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateTiers rejected hot max_used_percent %v: %v", tt.max, err)
			}
		})
	}
}

func TestValidateTiers_RejectsNonFiniteColdPercentages(t *testing.T) {
	tests := []struct {
		name   string
		target float64
		max    float64
	}{
		{name: "NaN target", target: math.NaN(), max: 95},
		{name: "infinite target", target: math.Inf(1), max: 95},
		{name: "NaN max", target: 90, max: math.NaN()},
		{name: "infinite max", target: 90, max: math.Inf(1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tier := model.Tier{
				Name: "cold", Role: model.RoleCold, Paths: []string{"/cold"},
				Media:             []model.MediaType{model.Movie},
				TargetUsedPercent: tt.target, MaxUsedPercent: tt.max,
			}
			if err := ValidateTiers([]model.Tier{tier}, false); err == nil {
				t.Fatalf("ValidateTiers accepted target=%v max=%v", tt.target, tt.max)
			}
		})
	}
}

// writeConfig writes body to a coldarr.yaml in a fresh temp dir and
// returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coldarr.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

const validTiersYAML = `
tiers:
  - name: hot
    role: hot
    paths: ["${COLDARR_TEST_HOT}"]
    media_types: [movie, tv]
  - name: cold
    role: cold
    paths: ["/mnt/cold"]
    media_types: [movie]
    max_used_percent: 95
`

// TestLoad_ExpandsEnvAndAppliesDefaults pins what the CLI gets back from a
// minimal config: ${VAR} references resolved, policy defaults filled in,
// and a cold tier with no target defaulting it to its own max.
func TestLoad_ExpandsEnvAndAppliesDefaults(t *testing.T) {
	t.Setenv("COLDARR_TEST_HOT", "/mnt/hot")
	cfg, err := Load(writeConfig(t, validTiersYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := cfg.Tiers[0].Paths[0]; got != "/mnt/hot" {
		t.Errorf("hot path = %q, want the ${COLDARR_TEST_HOT} reference expanded to /mnt/hot", got)
	}
	if cfg.Policy.CooldownDays != 30 || cfg.Policy.HotGraceDays != 14 || cfg.Policy.ColdScoreThreshold != 40 {
		t.Errorf("policy defaults not applied: %+v", cfg.Policy)
	}
	if cfg.History.Path != "./coldarr-history.json" {
		t.Errorf("History.Path = %q, want the default ./coldarr-history.json", cfg.History.Path)
	}
	if cold := cfg.Tiers[1]; cold.TargetUsedPercent != 95 {
		t.Errorf("cold TargetUsedPercent = %v, want it defaulted to max_used_percent (95)", cold.TargetUsedPercent)
	}
}

// TestLoad_RequiresHotAndColdButLoadForServerDoesNot pins the split between
// the two loaders: the CLI is about to plan and needs both roles, while the
// web GUI has to start on a half-configured install to finish setting it up.
func TestLoad_RequiresHotAndColdButLoadForServerDoesNot(t *testing.T) {
	path := writeConfig(t, `
tiers:
  - name: hot
    role: hot
    paths: ["/mnt/hot"]
    media_types: [movie]
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), `role "cold" is required`) {
		t.Fatalf("Load with no cold tier = %v, want a missing-cold-tier error", err)
	}

	cfg, err := LoadForServer(path)
	if err != nil {
		t.Fatalf("LoadForServer: %v", err)
	}
	if len(cfg.Tiers) != 1 || cfg.Tiers[0].Name != "hot" {
		t.Fatalf("LoadForServer tiers = %+v, want the single hot tier", cfg.Tiers)
	}
}

func TestLoad_RejectsBrokenFiles(t *testing.T) {
	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantErr string
	}{
		{
			name:    "missing file",
			path:    func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.yaml") },
			wantErr: "reading config",
		},
		{
			name:    "malformed YAML",
			path:    func(t *testing.T) string { return writeConfig(t, "tiers: [unclosed") },
			wantErr: "parsing config",
		},
		{
			name: "invalid tier",
			path: func(t *testing.T) string {
				return writeConfig(t, "tiers:\n  - name: hot\n    role: hot\n    paths: [relative/path]\n    media_types: [movie]\n")
			},
			wantErr: "must be absolute",
		},
		{
			name: "invalid schedule",
			path: func(t *testing.T) string {
				t.Setenv("COLDARR_TEST_HOT", "/mnt/hot")
				return writeConfig(t, validTiersYAML+"scheduler:\n  rescan_cold:\n    enabled: true\n    unit: daily\n    at: \"25:00\"\n")
			},
			wantErr: "scheduler.rescan_cold",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.path(t))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestLoadForServer_MissingFileIsAFreshInstall: a brand new /config volume
// has no coldarr.yaml yet, and the web GUI must still start - with every
// default in place - so the operator can configure it from scratch.
func TestLoadForServer_MissingFileIsAFreshInstall(t *testing.T) {
	cfg, err := LoadForServer(filepath.Join(t.TempDir(), "coldarr.yaml"))
	if err != nil {
		t.Fatalf("LoadForServer: %v", err)
	}
	if len(cfg.Tiers) != 0 {
		t.Errorf("fresh install tiers = %+v, want none", cfg.Tiers)
	}
	if cfg.Policy.CooldownDays != 30 || cfg.Scheduler.RunPlan.At != "03:00" {
		t.Errorf("fresh install should still carry defaults, got policy %+v, run_plan %+v", cfg.Policy, cfg.Scheduler.RunPlan)
	}
}

func TestLoadForServer_RejectsInvalidContent(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "malformed YAML", body: "tiers: [unclosed", wantErr: "parsing config"},
		{name: "duplicate tier", body: "tiers:\n  - {name: a, role: hot, paths: [/a], media_types: [movie]}\n  - {name: a, role: hot, paths: [/b], media_types: [movie]}\n", wantErr: `duplicate tier name "a"`},
		{name: "bad omit day", body: "scheduler:\n  weekly_omit_days: [funday]\n", wantErr: "scheduler.weekly_omit_days"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadForServer(writeConfig(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadForServer error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestSave_RoundTripsAndKeepsBackup covers what the web GUI relies on for
// every settings change: the file it writes loads back to the same config,
// a missing parent directory is created, and each save keeps the previous
// file as .bak in case the change turns out to be wrong.
func TestSave_RoundTripsAndKeepsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config", "coldarr.yaml")

	first := &Config{Tiers: []model.Tier{
		{Name: "hot", Role: model.RoleHot, Paths: []string{"/mnt/hot"}, Media: []model.MediaType{model.Movie}},
	}}
	if err := Save(path, first); err != nil {
		t.Fatalf("Save (first): %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("first save must not invent a backup, stat err = %v", err)
	}
	firstBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	second := &Config{Tiers: append(first.Tiers, model.Tier{
		Name: "cold", Role: model.RoleCold, Paths: []string{"/mnt/cold"}, Media: []model.MediaType{model.Movie},
		MaxUsedPercent: 95, TargetUsedPercent: 90,
	})}
	if err := Save(path, second); err != nil {
		t.Fatalf("Save (second): %v", err)
	}

	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("reading backup: %v", err)
	}
	if string(backup) != string(firstBytes) {
		t.Errorf("backup = %q, want the previous file's exact contents %q", backup, firstBytes)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if len(loaded.Tiers) != 2 || loaded.Tiers[1].Name != "cold" || loaded.Tiers[1].TargetUsedPercent != 90 {
		t.Fatalf("loaded tiers = %+v, want both saved tiers back", loaded.Tiers)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("Save left its temp file behind, stat err = %v", err)
	}
}

func TestSave_ReportsUnwritableLocation(t *testing.T) {
	// A regular file where the config directory should be: MkdirAll can't
	// create a directory beneath it, whoever the test runs as.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err := Save(filepath.Join(blocker, "coldarr.yaml"), &Config{})
	if err == nil || !strings.Contains(err.Error(), "creating") {
		t.Fatalf("Save under a regular file = %v, want a directory creation error", err)
	}
}

func TestValidateTiers_StructuralErrors(t *testing.T) {
	hot := model.Tier{Name: "hot", Role: model.RoleHot, Paths: []string{"/hot"}, Media: []model.MediaType{model.Movie}}
	cold := model.Tier{Name: "cold", Role: model.RoleCold, Paths: []string{"/cold"}, Media: []model.MediaType{model.Movie}, MaxUsedPercent: 95, TargetUsedPercent: 90}
	with := func(base model.Tier, edit func(*model.Tier)) model.Tier {
		base.Paths = append([]string(nil), base.Paths...)
		base.Media = append([]model.MediaType(nil), base.Media...)
		edit(&base)
		return base
	}

	tests := []struct {
		name              string
		tiers             []model.Tier
		requireHotAndCold bool
		wantErr           string
	}{
		{name: "no tiers when planning", tiers: nil, requireHotAndCold: true, wantErr: "at least one tier must be configured"},
		{name: "missing name", tiers: []model.Tier{with(hot, func(t *model.Tier) { t.Name = "" })}, wantErr: "tier missing name"},
		{name: "duplicate name", tiers: []model.Tier{hot, with(hot, func(t *model.Tier) { t.Paths = []string{"/hot2"} })}, wantErr: `duplicate tier name "hot"`},
		{name: "unknown role", tiers: []model.Tier{with(hot, func(t *model.Tier) { t.Role = "warm" })}, wantErr: `got "warm"`},
		{name: "no paths", tiers: []model.Tier{with(hot, func(t *model.Tier) { t.Paths = nil })}, wantErr: "at least one path"},
		{name: "relative path", tiers: []model.Tier{with(hot, func(t *model.Tier) { t.Paths = []string{"media/hot"} })}, wantErr: `path "media/hot" must be absolute`},
		{name: "path shared by two tiers", tiers: []model.Tier{hot, with(cold, func(t *model.Tier) { t.Paths = []string{"/hot"} })}, wantErr: `path "/hot" used by both tier "hot" and tier "cold"`},
		{name: "no media", tiers: []model.Tier{with(hot, func(t *model.Tier) { t.Media = nil })}, wantErr: "at least one media type"},
		{name: "unknown media", tiers: []model.Tier{with(hot, func(t *model.Tier) { t.Media = []model.MediaType{"music"} })}, wantErr: `unknown media type "music"`},
		{name: "cold target above max", tiers: []model.Tier{with(cold, func(t *model.Tier) { t.TargetUsedPercent = 96 })}, wantErr: "target_used_percent"},
		{name: "no hot tier when planning", tiers: []model.Tier{cold}, requireHotAndCold: true, wantErr: `role "hot" is required`},
		{name: "no cold tier when planning", tiers: []model.Tier{hot}, requireHotAndCold: true, wantErr: `role "cold" is required`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTiers(tt.tiers, tt.requireHotAndCold)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateTiers error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}

	if err := ValidateTiers([]model.Tier{hot, cold}, true); err != nil {
		t.Fatalf("ValidateTiers(hot, cold) = %v, want nil", err)
	}
	if err := ValidateTiers(nil, false); err != nil {
		t.Fatalf("ValidateTiers(nil) for the GUI's fresh install = %v, want nil", err)
	}
}

// TestValidateScheduler_NamesTheBrokenTask: a scheduler error has to say
// which task's schedule is wrong, or an operator editing coldarr.yaml by
// hand is left to guess among five near-identical blocks.
func TestValidateScheduler_NamesTheBrokenTask(t *testing.T) {
	broken := scheduler.Schedule{Enabled: true, Unit: "fortnightly", Every: 1}
	tests := []struct {
		task string
		cfg  SchedulerConfig
	}{
		{"run_plan", SchedulerConfig{RunPlan: broken}},
		{"rescan_cold", SchedulerConfig{RescanCold: broken}},
		{"refresh_links", SchedulerConfig{RefreshLinks: broken}},
		{"scan_cutoffs", SchedulerConfig{ScanCutoffs: broken}},
		{"scan_orphans", SchedulerConfig{ScanOrphans: broken}},
	}
	for _, tt := range tests {
		t.Run(tt.task, func(t *testing.T) {
			err := ValidateScheduler(tt.cfg)
			if err == nil || !strings.HasPrefix(err.Error(), "scheduler."+tt.task+":") {
				t.Fatalf("ValidateScheduler error = %v, want it prefixed with scheduler.%s:", err, tt.task)
			}
		})
	}
}
