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
