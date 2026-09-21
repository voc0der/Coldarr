package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// fixture is the whole fake environment: the drives Coldarr's tiers live
// on, and the library Radarr/Sonarr/Jellyfin report. See fixture.yaml.
type fixture struct {
	Versions struct {
		Radarr   string `yaml:"radarr"`
		Sonarr   string `yaml:"sonarr"`
		Jellyfin string `yaml:"jellyfin"`
	} `yaml:"versions"`
	Simulation simulation   `yaml:"simulation"`
	Drives     []driveSpec  `yaml:"drives"`
	Movies     []itemSpec   `yaml:"movies"`
	Series     []itemSpec   `yaml:"series"`
	Orphans    []orphanSpec `yaml:"orphans"`
}

// simulation controls how long a fake Radarr/Sonarr move takes. Real moves
// take minutes to hours; these are compressed so an apply finishes in about
// a minute but still spends long enough "moving" to be captured mid-run.
type simulation struct {
	MoveThroughput byteSize `yaml:"move_throughput"` // simulated bytes per second
	MinMoveSeconds float64  `yaml:"min_move_seconds"`
	MaxMoveSeconds float64  `yaml:"max_move_seconds"`
}

type driveSpec struct {
	Name        string   `yaml:"name"`
	Mount       string   `yaml:"mount"`
	Size        byteSize `yaml:"size"`
	UsedPercent float64  `yaml:"used_percent"`
}

type itemSpec struct {
	Title        string   `yaml:"title"`
	Year         int      `yaml:"year"`
	Root         string   `yaml:"root"`
	Size         byteSize `yaml:"size"`
	AddedDaysAgo float64  `yaml:"added_days_ago"`
	Profile      string   `yaml:"profile"`
	Monitored    *bool    `yaml:"monitored"`
	Tags         []string `yaml:"tags"`
	// Status is Radarr's movie status (released, inCinemas, announced, tba)
	// or Sonarr's series status (continuing, ended, upcoming).
	Status      string `yaml:"status"`
	Favorite    bool   `yaml:"favorite"`
	CutoffUnmet bool   `yaml:"cutoff_unmet"`
	Downloading bool   `yaml:"downloading"`
	// Series only.
	Seasons          int      `yaml:"seasons"`
	LastAiredDaysAgo *float64 `yaml:"last_aired_days_ago"`
	// MovedDaysAgo seeds a history record for an item already on a cold
	// tier, as if Coldarr moved it there that long ago.
	MovedDaysAgo *float64 `yaml:"moved_days_ago"`
}

type orphanSpec struct {
	Path string   `yaml:"path"`
	Size byteSize `yaml:"size"`
}

func loadFixture(path string) (*fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fx fixture
	if err := yaml.Unmarshal(data, &fx); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if fx.Simulation.MoveThroughput <= 0 {
		fx.Simulation.MoveThroughput = 4 << 30
	}
	if fx.Simulation.MinMoveSeconds <= 0 {
		fx.Simulation.MinMoveSeconds = 3
	}
	if fx.Simulation.MaxMoveSeconds < fx.Simulation.MinMoveSeconds {
		fx.Simulation.MaxMoveSeconds = fx.Simulation.MinMoveSeconds
	}
	if len(fx.Drives) == 0 {
		return nil, fmt.Errorf("%s: no drives", path)
	}
	for _, d := range fx.Drives {
		if d.Name == "" || !filepath.IsAbs(d.Mount) || d.Size <= 0 {
			return nil, fmt.Errorf("%s: drive %q needs a name, an absolute mount and a size", path, d.Name)
		}
	}
	return &fx, nil
}

// tierSpec is the slice of Coldarr's own tier config the gallery needs:
// which directories to create on each drive, and which tier a seeded
// history record moved an item between.
type tierSpec struct {
	Name       string   `yaml:"name"`
	Role       string   `yaml:"role"`
	Paths      []string `yaml:"paths"`
	MediaTypes []string `yaml:"media_types"`
}

func loadTiers(path string) ([]tierSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Tiers []tierSpec `yaml:"tiers"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg.Tiers, nil
}

// byteSize parses "58.4GiB", "16TB", "900MB" or a plain byte count.
// Decimal units (GB, TB) are what drives are sold in, so a "16TB" drive
// shows up in Coldarr as the 14901.2 GB a real one does; binary units
// (GiB, TiB) are what Coldarr displays, so "58.4GiB" shows as "58.4 GB".
type byteSize int64

var sizePattern = regexp.MustCompile(`^\s*([0-9]*\.?[0-9]+)\s*([KMGTP]i?B|B)?\s*$`)

var sizeUnits = map[string]float64{
	"": 1, "B": 1,
	"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12, "PB": 1e15,
	"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "PiB": 1 << 50,
}

func (s *byteSize) UnmarshalYAML(n *yaml.Node) error {
	m := sizePattern.FindStringSubmatch(n.Value)
	if m == nil {
		return fmt.Errorf("line %d: %q is not a size (e.g. 58.4GiB, 16TB)", n.Line, n.Value)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*s = byteSize(v * sizeUnits[m[2]])
	return nil
}

func gib(n uint64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/(1<<30)), ".0") + " GiB"
}
