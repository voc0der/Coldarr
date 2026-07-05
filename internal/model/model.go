// Package model holds the core domain types shared across Coldarr's
// inventory, scoring, planning, and mover packages.
package model

import "time"

// MediaType identifies the kind of library item.
type MediaType string

const (
	Movie MediaType = "movie"
	TV    MediaType = "tv"
)

// TierRole distinguishes primary ("hot") storage from overflow ("cold")
// storage. Hot tiers are monitored for pressure (usage above their max);
// cold tiers are packing targets (filled toward their target/max usage).
type TierRole string

const (
	RoleHot  TierRole = "hot"
	RoleCold TierRole = "cold"
)

// Tier is a named storage policy applied to one or more physical paths.
// Each path is evaluated independently against the same target/max usage
// thresholds - a tier is a policy shared by a set of mount points, not a
// single pooled volume.
type Tier struct {
	Name  string      `yaml:"name"`
	Role  TierRole    `yaml:"role"`
	Paths []string    `yaml:"paths"`
	Media []MediaType `yaml:"media_types"`
	// TargetUsedPercent is the steady-state usage Coldarr tries to
	// restore (hot) or pack toward (cold).
	TargetUsedPercent float64 `yaml:"target_used_percent"`
	// MaxUsedPercent is the ceiling that triggers offloading (hot) or
	// that a destination must never be pushed past (cold). Coldarr does
	// not clamp this value - set it to 100 if that's what you want.
	MaxUsedPercent float64 `yaml:"max_used_percent"`
	// RequireMount, when true, makes Coldarr refuse to treat a path as
	// usable storage unless it is a distinct mount point from its
	// parent directory. This guards against a satellite drive being
	// unmounted and Coldarr silently writing (or planning to write)
	// into the empty directory left behind on the root filesystem.
	RequireMount bool `yaml:"require_mount"`
}

// AcceptsMediaType reports whether this tier is configured to hold the
// given media type.
func (t Tier) AcceptsMediaType(mt MediaType) bool {
	for _, m := range t.Media {
		if m == mt {
			return true
		}
	}
	return false
}

// MediaItem is a normalized view of a Radarr movie or Sonarr series,
// independent of which Arr app it came from.
type MediaItem struct {
	// ArrApp is "radarr" or "sonarr" - identifies which app owns this
	// item and must be used to perform any move.
	ArrApp string
	ID     int
	Type   MediaType
	Title  string

	// Path is the item's current full folder path on disk as reported
	// by the owning Arr app.
	Path string
	// RootFolderPath is the root folder the item currently lives under.
	RootFolderPath string

	SizeBytes int64
	Added     time.Time
	Tags      []string

	QualityProfileName string
	Monitored          bool
	HasFile            bool

	// Ended is only meaningful for TV: true once a series has finished
	// airing (Sonarr status "ended"), false while continuing/upcoming.
	Ended bool
	// LastAired is the most recent known air date for a series, if any.
	LastAired *time.Time

	// InActiveQueue is true if the Arr app currently has an active
	// download, import, or upgrade in progress for this item. Items in
	// this state must never be moved.
	InActiveQueue bool
}

// Key uniquely identifies an item across app restarts, for history/cooldown
// lookups.
type Key struct {
	ArrApp string
	ID     int
}

func (m MediaItem) Key() Key {
	return Key{ArrApp: m.ArrApp, ID: m.ID}
}
