package model

import "testing"

func TestTier_AcceptsMediaType(t *testing.T) {
	tier := Tier{Media: []MediaType{Movie}}

	if !tier.AcceptsMediaType(Movie) {
		t.Error("expected tier to accept Movie")
	}
	if tier.AcceptsMediaType(TV) {
		t.Error("expected tier not to accept TV")
	}
}

func TestTier_AcceptsMediaType_Empty(t *testing.T) {
	var tier Tier
	if tier.AcceptsMediaType(Movie) {
		t.Error("a tier with no configured media types should accept nothing")
	}
}

func TestMediaItem_Key(t *testing.T) {
	item := MediaItem{ArrApp: "radarr", ID: 42}
	key := item.Key()

	if key.ArrApp != "radarr" || key.ID != 42 {
		t.Fatalf("Key() = %+v, want {radarr 42}", key)
	}

	other := MediaItem{ArrApp: "sonarr", ID: 42}
	if item.Key() == other.Key() {
		t.Error("items from different Arr apps with the same numeric ID must not produce equal keys")
	}
}

// TestTier_EffectiveMaxUsedPercent: an unset hot ceiling means the
// built-in reclaim default, never "no ceiling", while a cold tier's max is
// always exactly what was configured.
func TestTier_EffectiveMaxUsedPercent(t *testing.T) {
	cases := []struct {
		tier Tier
		want float64
	}{
		{Tier{Role: RoleHot}, DefaultHotMaxUsedPercent},
		{Tier{Role: RoleHot, MaxUsedPercent: 92.5}, 92.5},
		{Tier{Role: RoleCold, MaxUsedPercent: 95}, 95},
		{Tier{Role: RoleCold}, 0},
	}
	for _, c := range cases {
		if got := c.tier.EffectiveMaxUsedPercent(); got != c.want {
			t.Errorf("%s tier with max %v: EffectiveMaxUsedPercent() = %v, want %v", c.tier.Role, c.tier.MaxUsedPercent, got, c.want)
		}
	}
}

func TestTierDomains(t *testing.T) {
	legacy := Tier{Name: "hot"}
	if legacy.EffectiveDomain() != DomainLibrary || legacy.IsDownloads() {
		t.Errorf("a tier without a domain = %q, want library", legacy.EffectiveDomain())
	}
	seed := Tier{Name: "seed", Domain: DomainDownloads}
	if !seed.IsDownloads() {
		t.Error("a downloads tier should report it")
	}

	tiers := []Tier{legacy, seed, {Name: "cold", Domain: DomainLibrary}}
	if got := TiersIn(tiers, DomainLibrary); len(got) != 2 || got[0].Name != "hot" || got[1].Name != "cold" {
		t.Errorf("TiersIn(library) = %+v, want hot then cold", got)
	}
	if got := TiersIn(tiers, DomainDownloads); len(got) != 1 || got[0].Name != "seed" {
		t.Errorf("TiersIn(downloads) = %+v, want seed", got)
	}
}
