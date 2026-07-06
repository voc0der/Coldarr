package config

import (
	"testing"

	"github.com/vocoder/coldarr/internal/scheduler"
)

func TestApplyDefaults_SchedulerStaysDisabled(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)

	for name, s := range map[string]scheduler.Schedule{
		"run_plan":    cfg.Scheduler.RunPlan,
		"rescan_cold": cfg.Scheduler.RescanCold,
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

func TestApplyDefaults_AuthOIDC(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)

	if cfg.Auth.OIDC.RequiredGroup != "coldarr" {
		t.Fatalf("RequiredGroup = %q, want coldarr", cfg.Auth.OIDC.RequiredGroup)
	}
	if cfg.Auth.OIDC.GroupsClaim != "groups" {
		t.Fatalf("GroupsClaim = %q, want groups", cfg.Auth.OIDC.GroupsClaim)
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
