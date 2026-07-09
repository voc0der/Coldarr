// Package config loads, validates, and saves Coldarr's coldarr.yaml -
// tiers and policy only. Radarr/Sonarr/Jellyfin connection info lives
// separately, encrypted, in internal/secrets - see that package for why.
package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/scheduler"
	"gopkg.in/yaml.v3"
)

type PolicyConfig struct {
	// CooldownDays is how long an item is left alone after being moved,
	// regardless of what its score says.
	CooldownDays int `yaml:"cooldown_days"`
	// MinMoveSizeGB filters out items too small to be worth the risk of
	// a move.
	MinMoveSizeGB float64 `yaml:"min_move_size_gb"`
	// HotGraceDays keeps recently-added items on hot storage regardless
	// of score.
	HotGraceDays int `yaml:"hot_grace_days"`

	ProtectedTags []string `yaml:"protected_tags"`
	ColdOkTags    []string `yaml:"cold_ok_tags"`
	NeverMoveTags []string `yaml:"never_move_tags"`

	ProtectContinuingSeries bool `yaml:"protect_continuing_series"`

	LowPriorityQualityProfiles []string `yaml:"low_priority_quality_profiles"`

	// ColdScoreThreshold is the score (see internal/scoring) an item
	// must reach before it is considered a cold candidate.
	ColdScoreThreshold float64 `yaml:"cold_score_threshold"`
}

type HistoryConfig struct {
	Path string `yaml:"path"`
}

// NotificationsConfig controls Coldarr's Apprise webhook notifications.
// The Apprise URL itself is not here - it's stored encrypted alongside
// the Radarr/Sonarr/Jellyfin connections in internal/secrets, since an
// Apprise URL can itself function as a bearer credential (e.g. a raw
// Discord/Slack webhook pasted directly instead of routed through a
// hosted Apprise API gateway).
type NotificationsConfig struct {
	// Verbose sends one additional notification per item (e.g. per moved
	// file, per cold-storage path checked) alongside the summary every
	// task always sends. Off by default to keep notifications low-noise.
	Verbose bool `yaml:"verbose"`
	// Markdown formats notification bodies with Markdown (bold labels,
	// code-spans for paths/titles/errors) and tells Apprise to render the
	// body as Markdown rather than plain text. Off by default since not
	// every Apprise target renders Markdown.
	Markdown bool `yaml:"markdown"`
	// Tag restricts delivery to whatever Apprise notification target(s)
	// are registered under this tag on the receiving end. Many Apprise
	// API deployments route entirely by tag and fail a request that
	// matches none - left blank, no tag is sent and Apprise's own
	// default routing applies. Not sensitive (just a routing label), so
	// unlike the Apprise URL itself, this lives in plain coldarr.yaml.
	Tag string `yaml:"tag"`
}

// SchedulerConfig holds the recurrence for each of Coldarr's four
// schedulable tasks. All default to disabled - an unattended apply,
// cold-storage rescan, Links-cache refresh, or quality-cutoff scan only
// ever runs if explicitly turned on.
type SchedulerConfig struct {
	RunPlan      scheduler.Schedule `yaml:"run_plan"`
	RescanCold   scheduler.Schedule `yaml:"rescan_cold"`
	RefreshLinks scheduler.Schedule `yaml:"refresh_links"`
	// ScanCutoffs refreshes internal/cutoffcache - which items have an
	// unmet quality-profile cutoff - by calling Radarr/Sonarr's
	// wanted/cutoff endpoint. Deliberately never done live on a
	// Dashboard/Plan page view (see cutoffcache's package doc for why),
	// so scoring only actually keeps a cutoff-unmet item on hot storage
	// once this has run at least once - enable it, or use its manual
	// "Scan now" button, for that protection to take effect.
	ScanCutoffs scheduler.Schedule `yaml:"scan_cutoffs"`
	// ScanOrphans refreshes internal/orphans - folders sitting on a tier
	// path that no service (Radarr, Sonarr, Jellyfin) still tracks - by
	// walking every configured tier's paths on disk. Deliberately never
	// done live on a page view (see orphans' package doc for why); the
	// Settings > Orphaned Storage page only ever shows the last scan's
	// results.
	ScanOrphans scheduler.Schedule `yaml:"scan_orphans"`
}

type AuthConfig struct {
	OIDC OIDCAuthConfig `yaml:"oidc"`
}

type OIDCAuthConfig struct {
	Enabled         bool   `yaml:"enabled"`
	IssuerURL       string `yaml:"issuer_url"`
	ClientID        string `yaml:"client_id"`
	RedirectURL     string `yaml:"redirect_url"`
	RequiredGroup   string `yaml:"required_group"`
	GroupsClaim     string `yaml:"groups_claim"`
	TokenAuthMethod string `yaml:"token_auth_method"`
	AutoLogin       bool   `yaml:"auto_login"`
}

type Config struct {
	Tiers         []model.Tier        `yaml:"tiers"`
	Policy        PolicyConfig        `yaml:"policy"`
	History       HistoryConfig       `yaml:"history"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Scheduler     SchedulerConfig     `yaml:"scheduler"`
	Auth          AuthConfig          `yaml:"auth"`
}

// Load reads, parses, and strictly validates the config file at path -
// used by the report/plan/apply CLI commands, which should fail fast and
// clearly if the config isn't ready to plan against. Values may reference
// environment variables via ${VAR}.
func Load(path string) (*Config, error) {
	cfg, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if err := ValidateTiers(cfg.Tiers, true); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if err := ValidateScheduler(cfg.Scheduler); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// LoadForServer reads and parses the config file at path like Load, but
// tolerates a missing file (returning an empty default config so a fresh
// /config volume can be configured for the first time through the web
// GUI) and does not require at least one hot and one cold tier to already
// exist - the GUI's job includes getting a new install to that state.
func LoadForServer(path string) (*Config, error) {
	if abs, err := filepath.Abs(path); err == nil {
		log.Printf("config: using config file %s (resolved from %q)", abs, path)
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Printf("config: no config file found at %s (fresh install, or none saved yet)", path)
		cfg := &Config{}
		applyDefaults(cfg)
		return cfg, nil
	}

	cfg, err := readFile(path)
	if err != nil {
		return nil, err
	}
	if err := ValidateTiers(cfg.Tiers, false); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if err := ValidateScheduler(cfg.Scheduler); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	log.Printf("config: loaded %d tier(s) from %s: %v", len(cfg.Tiers), path, tierNames(cfg.Tiers))
	return cfg, nil
}

// ValidateScheduler checks both of the scheduler's task schedules -
// invoked wherever config is loaded, and again before saving a schedule
// change through the web GUI, so a malformed Every/At value never reaches
// coldarr.yaml.
func ValidateScheduler(cfg SchedulerConfig) error {
	if err := scheduler.Validate(cfg.RunPlan); err != nil {
		return fmt.Errorf("scheduler.run_plan: %w", err)
	}
	if err := scheduler.Validate(cfg.RescanCold); err != nil {
		return fmt.Errorf("scheduler.rescan_cold: %w", err)
	}
	if err := scheduler.Validate(cfg.RefreshLinks); err != nil {
		return fmt.Errorf("scheduler.refresh_links: %w", err)
	}
	if err := scheduler.Validate(cfg.ScanCutoffs); err != nil {
		return fmt.Errorf("scheduler.scan_cutoffs: %w", err)
	}
	if err := scheduler.Validate(cfg.ScanOrphans); err != nil {
		return fmt.Errorf("scheduler.scan_orphans: %w", err)
	}
	return nil
}

// applyScheduleDefaults fills in cosmetic defaults for a not-yet-configured
// schedule, so its form has sane pre-filled values before an operator ever
// enables it. It never touches Enabled - that stays false (Go's zero
// value) until someone explicitly turns the task on.
func applyScheduleDefaults(s *scheduler.Schedule, defaultAt string) {
	if s.Unit == "" {
		s.Unit = scheduler.Daily
	}
	if s.Every == 0 {
		s.Every = 1
	}
	if s.At == "" {
		s.At = defaultAt
	}
}

func tierNames(tiers []model.Tier) []string {
	names := make([]string, len(tiers))
	for i, t := range tiers {
		names[i] = t.Name
	}
	return names
}

func readFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	expanded := os.ExpandEnv(string(raw))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	applyDefaults(&cfg)
	return &cfg, nil
}

// Save writes cfg back to path as YAML, atomically, keeping a copy of
// whatever was there before as path+".bak" - a cheap recovery path if a
// save turns out to be wrong for any reason. Note this rewrites the whole
// file - hand-added comments will not survive a save made through the web
// GUI.
func Save(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	if existing, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", existing, 0o600); err != nil { //nolint:gosec // path is the server's own configured config file location, never derived from a request
			return fmt.Errorf("backing up %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading %s to back it up: %w", path, err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("saving %s: %w", path, err)
	}

	abs, absErr := filepath.Abs(path)
	if absErr != nil {
		abs = path
	}
	log.Printf("config: saved %d tier(s) to %s: %v", len(cfg.Tiers), abs, tierNames(cfg.Tiers))
	return nil
}

func applyDefaults(cfg *Config) {
	if cfg.Policy.CooldownDays == 0 {
		cfg.Policy.CooldownDays = 30
	}
	if cfg.Policy.HotGraceDays == 0 {
		cfg.Policy.HotGraceDays = 14
	}
	if cfg.Policy.ColdScoreThreshold == 0 {
		cfg.Policy.ColdScoreThreshold = 40
	}
	if cfg.History.Path == "" {
		cfg.History.Path = "./coldarr-history.json"
	}
	if cfg.Auth.OIDC.RequiredGroup == "" {
		cfg.Auth.OIDC.RequiredGroup = "coldarr"
	}
	if cfg.Auth.OIDC.GroupsClaim == "" {
		cfg.Auth.OIDC.GroupsClaim = "groups"
	}
	if cfg.Auth.OIDC.TokenAuthMethod == "" {
		cfg.Auth.OIDC.TokenAuthMethod = "auto"
	}
	applyScheduleDefaults(&cfg.Scheduler.RunPlan, "03:00")
	applyScheduleDefaults(&cfg.Scheduler.RescanCold, "02:00")
	applyScheduleDefaults(&cfg.Scheduler.RefreshLinks, "01:00")
	applyScheduleDefaults(&cfg.Scheduler.ScanCutoffs, "00:30")
	applyScheduleDefaults(&cfg.Scheduler.ScanOrphans, "00:45")
	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		if t.Role == model.RoleCold && t.TargetUsedPercent == 0 {
			t.TargetUsedPercent = t.MaxUsedPercent
		}
	}
}

// ValidateTiers checks tiers for structural correctness (unique names,
// absolute non-overlapping paths, valid roles/media types/thresholds). If
// requireHotAndCold is true, it also requires at least one tier of each
// role - appropriate when about to build a plan, not appropriate while a
// fresh install is still being configured one tier at a time through the
// GUI.
func ValidateTiers(tiers []model.Tier, requireHotAndCold bool) error {
	if requireHotAndCold && len(tiers) == 0 {
		return fmt.Errorf("at least one tier must be configured")
	}

	var haveHot, haveCold bool
	seenNames := map[string]bool{}
	seenPaths := map[string]string{}

	for _, t := range tiers {
		if t.Name == "" {
			return fmt.Errorf("tier missing name")
		}
		if seenNames[t.Name] {
			return fmt.Errorf("duplicate tier name %q", t.Name)
		}
		seenNames[t.Name] = true

		switch t.Role {
		case model.RoleHot:
			haveHot = true
		case model.RoleCold:
			haveCold = true
		default:
			return fmt.Errorf("tier %q: role must be %q or %q, got %q", t.Name, model.RoleHot, model.RoleCold, t.Role)
		}

		if len(t.Paths) == 0 {
			return fmt.Errorf("tier %q: must configure at least one path", t.Name)
		}
		for _, p := range t.Paths {
			if !strings.HasPrefix(p, "/") {
				return fmt.Errorf("tier %q: path %q must be absolute", t.Name, p)
			}
			if owner, ok := seenPaths[p]; ok {
				return fmt.Errorf("path %q used by both tier %q and tier %q", p, owner, t.Name)
			}
			seenPaths[p] = t.Name
		}

		if len(t.Media) == 0 {
			return fmt.Errorf("tier %q: must configure at least one media type (movie, tv)", t.Name)
		}
		for _, mt := range t.Media {
			if mt != model.Movie && mt != model.TV {
				return fmt.Errorf("tier %q: unknown media type %q", t.Name, mt)
			}
		}

		// target/max are only meaningful for cold tiers - hot storage is
		// runoff, not something Coldarr steers toward a usage level.
		if t.Role == model.RoleCold {
			if t.MaxUsedPercent <= 0 || t.MaxUsedPercent > 100 {
				return fmt.Errorf("tier %q: max_used_percent must be in (0, 100], got %v", t.Name, t.MaxUsedPercent)
			}
			if t.TargetUsedPercent <= 0 || t.TargetUsedPercent > t.MaxUsedPercent {
				return fmt.Errorf("tier %q: target_used_percent must be in (0, max_used_percent], got %v", t.Name, t.TargetUsedPercent)
			}
		}
	}

	if requireHotAndCold {
		if !haveHot {
			return fmt.Errorf("at least one tier with role %q is required", model.RoleHot)
		}
		if !haveCold {
			return fmt.Errorf("at least one tier with role %q is required", model.RoleCold)
		}
	}

	return nil
}
