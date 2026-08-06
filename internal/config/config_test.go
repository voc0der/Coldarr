package config

import (
	"math"
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
		RunPlan:    scheduler.Schedule{Enabled: true, Unit: scheduler.Daily, Every: 1, At: "03:00"},
		RescanCold: scheduler.Schedule{Enabled: false},
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
