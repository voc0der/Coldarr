package webui

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vocoder/coldarr/internal/model"
)

func TestDashboardTemplate_ShowsEffectiveHotCeiling(t *testing.T) {
	pages, err := parseTemplates()
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}
	defaultTier := model.Tier{Role: model.RoleHot}
	overrideTier := model.Tier{Role: model.RoleHot, MaxUsedPercent: 99}
	defaultRow := dashboardTierRow(defaultTier, "/hot-default")
	defaultRow.TierName = "default"
	defaultRow.Available = true
	overrideRow := dashboardTierRow(overrideTier, "/hot-override")
	overrideRow.TierName = "override"
	overrideRow.Available = true
	data := dashboardData{Title: "Dashboard", Rows: []tierRow{
		defaultRow,
		overrideRow,
	}}

	s := &Server{pages: pages}
	rec := httptest.NewRecorder()
	s.render(rec, "dashboard", data)
	page := rec.Body.String()
	if !strings.Contains(page, "reclaim max 97.0% (default)") {
		t.Fatalf("dashboard does not show the effective default hot ceiling: %s", page)
	}
	if !strings.Contains(page, "reclaim max 99.0% (configured)") {
		t.Fatalf("dashboard does not distinguish an explicit hot ceiling: %s", page)
	}
}

func TestDashboardHotUsageAboveReclaimMaxIsNotAnError(t *testing.T) {
	row := dashboardTierRow(model.Tier{Role: model.RoleHot}, "/hot")
	row.UsedPercent = 98.6
	if usagePastHardLimit(row) {
		t.Fatal("hot usage above the reclaim admission limit is valid runoff, not an over-max error")
	}
}
