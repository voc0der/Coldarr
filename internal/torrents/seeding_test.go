package torrents

import (
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/qbit"
)

type trackerMap map[string][]string

func (m trackerMap) Hosts(hash string) ([]string, bool) {
	h, ok := m[hash]
	return h, ok
}

func seedingConfig() config.SeedingConfig {
	grace := 7
	return config.SeedingConfig{MinSeedDays: 14, GraceDays: &grace, Overrides: []config.TrackerOverride{
		{Host: "example.org", MinSeedDays: 21},
		{Host: "strict.example.org", MinSeedDays: 60},
		{Host: "forever.example.net", MinSeedDays: 0},
	}}
}

func seeded(days float64) qbit.Torrent {
	d := time.Duration(days * 24 * float64(time.Hour))
	return qbit.Torrent{Hash: "h", SeedingTime: d, SeedingTimeKnown: true, CompletedOn: now.Add(-d)}
}

func TestSeedingStatus(t *testing.T) {
	cfg := seedingConfig()
	day := 24 * time.Hour
	tests := []struct {
		name     string
		torrent  qbit.Torrent
		hosts    []string
		unknown  bool
		required time.Duration
		forever  bool
		met      bool
		rule     string
		reason   string
	}{
		{name: "default, not yet", torrent: seeded(20), hosts: []string{"other.net"}, required: 21 * day, rule: "default: 14 days"},
		{name: "default, met", torrent: seeded(21), hosts: []string{"other.net"}, required: 21 * day, met: true},
		{name: "override by subdomain", torrent: seeded(28), hosts: []string{"tracker.example.org"}, required: 28 * day, met: true, rule: "example.org: 21 days"},
		{name: "most specific override wins", torrent: seeded(30), hosts: []string{"a.strict.example.org"}, required: 67 * day, rule: "strict.example.org: 60 days"},
		{name: "strictest tracker wins", torrent: seeded(30), hosts: []string{"other.net", "tracker.example.org"}, required: 28 * day, met: true, rule: "example.org: 21 days"},
		{name: "forever wins", torrent: seeded(999), hosts: []string{"tracker.example.org", "forever.example.net"}, forever: true, rule: "forever.example.net: forever"},
		{name: "no trackers gets the default", torrent: seeded(30), hosts: []string{}, required: 21 * day, met: true, rule: "default: 14 days"},
		{name: "not scanned", torrent: seeded(99), unknown: true, reason: "haven't been scanned"},
		{name: "no seeding time", torrent: qbit.Torrent{Hash: "h", CompletedOn: now.Add(-99 * day)}, hosts: []string{"other.net"}, unknown: true, reason: "doesn't report how long"},
	}
	for _, tt := range tests {
		trackers := trackerMap{}
		if tt.hosts != nil {
			trackers["h"] = tt.hosts
		}
		got := seedingStatus(tt.torrent, trackers, cfg, now)
		switch {
		case tt.unknown:
			if got.Known || got.Met || !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("%s: %+v, want unknown because %q", tt.name, got, tt.reason)
			}
		case got.Forever != tt.forever || got.Required != tt.required || got.Met != tt.met:
			t.Errorf("%s: %+v, want required %v forever %v met %v", tt.name, got, tt.required, tt.forever, tt.met)
		case tt.rule != "" && got.Rule != tt.rule:
			t.Errorf("%s: rule = %q, want %q", tt.name, got.Rule, tt.rule)
		}
	}
}

// TestSeedingStatus_TakesTheSmallerClock: seeding time can reset, and time
// since finishing includes time stopped - the smaller one counts.
func TestSeedingStatus_TakesTheSmallerClock(t *testing.T) {
	trackers := trackerMap{"h": {"other.net"}}
	reset := qbit.Torrent{Hash: "h", SeedingTime: 2 * 24 * time.Hour, SeedingTimeKnown: true, CompletedOn: now.Add(-30 * 24 * time.Hour)}
	if got := seedingStatus(reset, trackers, seedingConfig(), now); got.Seeded != 2*24*time.Hour || got.Met {
		t.Errorf("reset seeding time = %+v, want 2 days counted", got)
	}
	skewed := qbit.Torrent{Hash: "h", SeedingTime: 30 * 24 * time.Hour, SeedingTimeKnown: true, CompletedOn: now.Add(-3 * 24 * time.Hour)}
	if got := seedingStatus(skewed, trackers, seedingConfig(), now); got.Seeded != 3*24*time.Hour {
		t.Errorf("seeding time longer than wall clock = %+v, want 3 days counted", got)
	}
	future := qbit.Torrent{Hash: "h", SeedingTime: time.Hour, SeedingTimeKnown: true, CompletedOn: now.Add(time.Hour)}
	if got := seedingStatus(future, trackers, seedingConfig(), now); got.Seeded != 0 {
		t.Errorf("a completion in the future = %+v, want nothing counted", got)
	}
	if got := seedingStatus(reset, nil, seedingConfig(), now); got.Known {
		t.Error("no tracker source at all means nothing is known")
	}
}

func TestSeedingStatus_DefaultForever(t *testing.T) {
	got := seedingStatus(seeded(500), trackerMap{"h": {"other.net"}}, config.SeedingConfig{}, now)
	if !got.Known || !got.Forever || got.Met || got.Rule != "default: forever" {
		t.Errorf("unconfigured default = %+v, want forever: nothing is ever done until someone says so", got)
	}
}

func TestClassify_SetsSeeding(t *testing.T) {
	in := input(settled("abc", "X", "manual"))
	in.Config.Seeding = seedingConfig()
	in.Trackers = trackerMap{"abc": {"other.net"}}
	d := Classify(in)[0]
	if !d.Seeding.Known || d.Seeding.Required != 21*24*time.Hour {
		t.Errorf("Seeding = %+v", d.Seeding)
	}
}
