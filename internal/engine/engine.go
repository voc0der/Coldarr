// Package engine wires together config, the Arr API clients, disk usage
// checks, scoring, and planning into the handful of operations the CLI
// commands need: build an inventory, build a plan from it, and hand off
// execution to the mover.
package engine

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/history"
	"github.com/vocoder/coldarr/internal/jellyfin"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/planner"
	"github.com/vocoder/coldarr/internal/scoring"
	"github.com/vocoder/coldarr/internal/secrets"
)

type Engine struct {
	Cfg          *config.Config
	Radarr       *arrapi.RadarrClient
	Sonarr       *arrapi.SonarrClient
	History      *history.Store
	jellyfinConn secrets.Connection
	jellyfinOK   bool
}

// New builds an Engine from a parsed config and the resolved connection
// store. It does not require any app to actually be configured - the web
// GUI needs to construct an Engine even on a completely fresh install so
// it can render the (empty) dashboard and let the operator add
// connections. Callers that need at least one library source (the CLI)
// should check e.Radarr == nil && e.Sonarr == nil themselves.
func New(cfg *config.Config, connStore *secrets.Store) (*Engine, error) {
	e := &Engine{Cfg: cfg}

	if conn, source := connStore.Effective("radarr"); source != secrets.SourceNone {
		e.Radarr = arrapi.NewRadarrClient(conn.URL, conn.APIKey)
	}
	if conn, source := connStore.Effective("sonarr"); source != secrets.SourceNone {
		e.Sonarr = arrapi.NewSonarrClient(conn.URL, conn.APIKey)
	}
	if conn, source := connStore.Effective("jellyfin"); source != secrets.SourceNone && conn.Enabled {
		e.jellyfinConn = conn
		e.jellyfinOK = true
	}

	hist, err := history.Load(cfg.History.Path)
	if err != nil {
		return nil, err
	}
	e.History = hist

	return e, nil
}

// PathStatus is the result of checking one configured tier path: either
// it's usable and Usage is populated, or Err explains why Coldarr is
// refusing to touch it (missing, not a directory, not the expected mount).
type PathStatus struct {
	Tier  model.Tier
	Path  string
	Usage diskusage.Usage
	Err   error

	// DeviceID identifies the underlying filesystem, so paths that are
	// really the same physical volume (however differently named or
	// nested) can be treated as one shared capacity pool instead of
	// independent ones. Only meaningful when DeviceIDKnown is true - a
	// failure here degrades to "don't group," not an error, since it's
	// not essential to using the path.
	DeviceID      uint64
	DeviceIDKnown bool
}

type Inventory struct {
	Tiers      []model.Tier
	PathStatus map[string]PathStatus
	Items      []planner.ItemEval
	// Warnings surfaces non-fatal problems encountered while building the
	// inventory (e.g. Jellyfin favorites couldn't be fetched) that the
	// operator should see but that shouldn't block report/plan/apply.
	Warnings []string
}

// UsableUsage returns usage for only the paths that passed their
// existence/mount checks. Paths that failed are simply absent - the
// planner treats "absent" as "cannot be used," never as "assume empty."
func (inv *Inventory) UsableUsage() map[string]diskusage.Usage {
	out := make(map[string]diskusage.Usage, len(inv.PathStatus))
	for path, status := range inv.PathStatus {
		if status.Err == nil {
			out[path] = status.Usage
		}
	}
	return out
}

// TierOf returns the configured tier a filesystem path belongs to, if any.
func (inv *Inventory) TierOf(path string) (model.Tier, bool) {
	status, ok := inv.PathStatus[path]
	if !ok {
		return model.Tier{}, false
	}
	return status.Tier, true
}

// VolumeOf returns, for every path with a known device ID, that device ID
// - for grouping paths that share a physical volume so the planner treats
// their capacity as one shared pool instead of independent ones.
func (inv *Inventory) VolumeOf() map[string]uint64 {
	out := make(map[string]uint64, len(inv.PathStatus))
	for path, status := range inv.PathStatus {
		if status.DeviceIDKnown {
			out[path] = status.DeviceID
		}
	}
	return out
}

// SharedVolumePaths returns every other configured path that is on the
// same physical volume as path, for surfacing in the UI.
func (inv *Inventory) SharedVolumePaths(path string) []string {
	status, ok := inv.PathStatus[path]
	if !ok || !status.DeviceIDKnown {
		return nil
	}
	var siblings []string
	for other, otherStatus := range inv.PathStatus {
		if other == path || !otherStatus.DeviceIDKnown {
			continue
		}
		if otherStatus.DeviceID == status.DeviceID {
			siblings = append(siblings, other)
		}
	}
	return siblings
}

// BuildInventory checks every configured path, fetches the library from
// every enabled Arr app, and scores each item. It performs no writes.
func (e *Engine) BuildInventory(now time.Time) (*Inventory, error) {
	inv := &Inventory{
		Tiers:      e.Cfg.Tiers,
		PathStatus: map[string]PathStatus{},
	}

	for _, tier := range e.Cfg.Tiers {
		for _, path := range tier.Paths {
			status := PathStatus{Tier: tier, Path: path}

			if err := diskusage.CheckPath(path, tier.RequireMount); err != nil {
				status.Err = err
			} else if u, err := diskusage.Stat(path); err != nil {
				status.Err = err
			} else {
				status.Usage = u
				if dev, err := diskusage.DeviceID(path); err == nil {
					status.DeviceID = dev
					status.DeviceIDKnown = true
				}
			}

			inv.PathStatus[path] = status
		}
	}

	var items []model.MediaItem

	if e.Radarr != nil {
		movies, err := e.Radarr.FetchMovies()
		if err != nil {
			return nil, fmt.Errorf("fetching movies from radarr: %w", err)
		}
		items = append(items, movies...)
	}

	if e.Sonarr != nil {
		series, err := e.Sonarr.FetchSeries()
		if err != nil {
			return nil, fmt.Errorf("fetching series from sonarr: %w", err)
		}
		items = append(items, series...)
	}

	var favorites map[string]bool
	if jf := e.JellyfinClient(); jf != nil {
		paths, err := jf.FavoritePaths()
		if err != nil {
			inv.Warnings = append(inv.Warnings, fmt.Sprintf("could not fetch Jellyfin favorites, favorited items are NOT protected this run: %v", err))
		} else {
			favorites = paths
		}
	}

	for _, it := range items {
		if favorites[filepath.Clean(it.Path)] {
			it.JellyfinFavorite = true
		}
		eval := scoring.Evaluate(it, e.Cfg.Policy, now)
		inv.Items = append(inv.Items, planner.ItemEval{Item: it, Eval: eval})
	}

	return inv, nil
}

// BuildPlan runs the planner over an inventory. It performs no writes.
func (e *Engine) BuildPlan(inv *Inventory, now time.Time) (*planner.Plan, error) {
	return planner.Build(planner.Input{
		Tiers:    inv.Tiers,
		Usage:    inv.UsableUsage(),
		VolumeOf: inv.VolumeOf(),
		Items:    inv.Items,
		History:  e.History,
		Policy:   e.Cfg.Policy,
		Now:      now,
	})
}

func (e *Engine) Movers() *mover.Movers {
	return &mover.Movers{Radarr: e.Radarr, Sonarr: e.Sonarr, History: e.History}
}

// JellyfinClient returns a client for the configured Jellyfin instance, or
// nil if Jellyfin rescan-on-move isn't configured.
func (e *Engine) JellyfinClient() *jellyfin.Client {
	if !e.jellyfinOK {
		return nil
	}
	return jellyfin.NewClient(e.jellyfinConn.URL, e.jellyfinConn.APIKey)
}
