package torrents

import (
	"fmt"
	"strings"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/qbit"
)

// Trackers answers which tracker hosts a download announces to, and
// whether that's known yet (see internal/trackercache).
type Trackers interface {
	Hosts(hash string) ([]string, bool)
}

// Seeding is how a download stands against its trackers' seeding
// requirement.
type Seeding struct {
	// Known is false until both the requirement and the time seeded can
	// be worked out - Reason says what's missing. An unknown requirement
	// is never met.
	Known  bool
	Reason string
	// Forever means a tracker requires seeding with no end.
	Forever bool
	// Required includes the grace margin.
	Required time.Duration
	Seeded   time.Duration
	Met      bool
	// Rule says which rule set the requirement.
	Rule string
}

// seedingStatus works out where a download stands against its trackers'
// requirements: the strictest of them, since it has to satisfy every
// tracker it announces to. A forever requirement from any of them wins.
//
// How long it has seeded is an approximation: trackers count their own
// announced time, which Coldarr can't see. qBittorrent's seeding time can
// reset (a recheck, a re-add), and wall-clock time since it finished
// includes time it was stopped, so the smaller of the two is used, and
// the grace margin covers what's left of the difference.
func seedingStatus(t qbit.Torrent, trackers Trackers, cfg config.SeedingConfig, now time.Time) Seeding {
	var hosts []string
	resolved := false
	if trackers != nil {
		hosts, resolved = trackers.Hosts(t.Hash)
	}
	if !resolved {
		return Seeding{Reason: "its trackers haven't been scanned yet"}
	}
	if !t.SeedingTimeKnown {
		return Seeding{Reason: "qBittorrent doesn't report how long it has seeded"}
	}

	seeded := t.SeedingTime
	if !t.CompletedOn.IsZero() {
		if wall := now.Sub(t.CompletedOn); wall < seeded {
			seeded = max(0, wall)
		}
	}

	days, forever, rule := requirement(hosts, cfg)
	s := Seeding{Known: true, Seeded: seeded, Forever: forever, Rule: rule}
	if forever {
		return s
	}
	s.Required = time.Duration(days+cfg.Grace()) * 24 * time.Hour
	s.Met = seeded >= s.Required
	return s
}

// requirement is the strictest requirement among hosts, in days, whether
// any of them is forever, and which rule set it. A download with no
// trackers at all (DHT only) gets the default.
func requirement(hosts []string, cfg config.SeedingConfig) (days int, forever bool, rule string) {
	if len(hosts) == 0 {
		return cfg.MinSeedDays, cfg.MinSeedDays == 0, describeRule("default", cfg.MinSeedDays)
	}
	first := true
	for _, host := range hosts {
		d, name := cfg.MinSeedDays, "default"
		if o, ok := override(host, cfg.Overrides); ok {
			d, name = o.MinSeedDays, o.Host
		}
		switch {
		case d == 0 && !forever:
			forever, rule = true, describeRule(name, 0)
		case !forever && (first || d > days):
			days, rule = d, describeRule(name, d)
		}
		first = false
	}
	return days, forever, rule
}

// override finds the most specific override for host: an exact match, or
// the longest host it's a subdomain of.
func override(host string, overrides []config.TrackerOverride) (config.TrackerOverride, bool) {
	var best config.TrackerOverride
	found := false
	for _, o := range overrides {
		if (host == o.Host || strings.HasSuffix(host, "."+o.Host)) && (!found || len(o.Host) > len(best.Host)) {
			best, found = o, true
		}
	}
	return best, found
}

func describeRule(name string, days int) string {
	if days == 0 {
		return name + ": forever"
	}
	return fmt.Sprintf("%s: %d days", name, days)
}
