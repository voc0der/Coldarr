// Package config loads and validates Coldarr's YAML configuration.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/vocoder/coldarr/internal/model"
	"gopkg.in/yaml.v3"
)

type ArrConfig struct {
	URL    string `yaml:"url"`
	APIKey string `yaml:"api_key"`
}

func (a *ArrConfig) enabled() bool {
	return a != nil && a.URL != "" && a.APIKey != ""
}

type JellyfinConfig struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"`
	APIKey  string `yaml:"api_key"`
}

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

type Config struct {
	Radarr   *ArrConfig      `yaml:"radarr"`
	Sonarr   *ArrConfig      `yaml:"sonarr"`
	Jellyfin *JellyfinConfig `yaml:"jellyfin"`

	Tiers []model.Tier `yaml:"tiers"`

	Policy  PolicyConfig  `yaml:"policy"`
	History HistoryConfig `yaml:"history"`
}

func (c *Config) RadarrEnabled() bool { return c.Radarr.enabled() }
func (c *Config) SonarrEnabled() bool { return c.Sonarr.enabled() }
func (c *Config) JellyfinEnabled() bool {
	return c.Jellyfin != nil && c.Jellyfin.Enabled && c.Jellyfin.URL != "" && c.Jellyfin.APIKey != ""
}

// Load reads, expands environment variables in, parses, and validates the
// config file at path.
func Load(path string) (*Config, error) {
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

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}

	return &cfg, nil
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
	for i := range cfg.Tiers {
		t := &cfg.Tiers[i]
		if t.TargetUsedPercent == 0 {
			t.TargetUsedPercent = t.MaxUsedPercent
		}
	}
}

func validate(cfg *Config) error {
	if !cfg.RadarrEnabled() && !cfg.SonarrEnabled() {
		return fmt.Errorf("at least one of radarr or sonarr must be configured with url + api_key")
	}

	if len(cfg.Tiers) == 0 {
		return fmt.Errorf("at least one tier must be configured")
	}

	var haveHot, haveCold bool
	seenNames := map[string]bool{}
	seenPaths := map[string]string{}

	for _, t := range cfg.Tiers {
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

		if t.MaxUsedPercent <= 0 || t.MaxUsedPercent > 100 {
			return fmt.Errorf("tier %q: max_used_percent must be in (0, 100], got %v", t.Name, t.MaxUsedPercent)
		}
		if t.TargetUsedPercent <= 0 || t.TargetUsedPercent > t.MaxUsedPercent {
			return fmt.Errorf("tier %q: target_used_percent must be in (0, max_used_percent], got %v", t.Name, t.TargetUsedPercent)
		}
	}

	if !haveHot {
		return fmt.Errorf("at least one tier with role %q is required", model.RoleHot)
	}
	if !haveCold {
		return fmt.Errorf("at least one tier with role %q is required", model.RoleCold)
	}

	return nil
}
