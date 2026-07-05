// Package engine wires together config, the Arr API clients, disk usage
// checks, scoring, and planning into the handful of operations the CLI
// commands need: build an inventory, build a plan from it, and hand off
// execution to the mover.
package engine

import (
	"fmt"
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
)

type Engine struct {
	Cfg     *config.Config
	Radarr  *arrapi.RadarrClient
	Sonarr  *arrapi.SonarrClient
	History *history.Store
}

func New(cfg *config.Config) (*Engine, error) {
	e := &Engine{Cfg: cfg}

	if cfg.RadarrEnabled() {
		e.Radarr = arrapi.NewRadarrClient(cfg.Radarr.URL, cfg.Radarr.APIKey)
	}
	if cfg.SonarrEnabled() {
		e.Sonarr = arrapi.NewSonarrClient(cfg.Sonarr.URL, cfg.Sonarr.APIKey)
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
}

type Inventory struct {
	Tiers      []model.Tier
	PathStatus map[string]PathStatus
	Items      []planner.ItemEval
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

	for _, it := range items {
		eval := scoring.Evaluate(it, e.Cfg.Policy, now)
		inv.Items = append(inv.Items, planner.ItemEval{Item: it, Eval: eval})
	}

	return inv, nil
}

// BuildPlan runs the planner over an inventory. It performs no writes.
func (e *Engine) BuildPlan(inv *Inventory, now time.Time) (*planner.Plan, error) {
	return planner.Build(planner.Input{
		Tiers:   inv.Tiers,
		Usage:   inv.UsableUsage(),
		Items:   inv.Items,
		History: e.History,
		Policy:  e.Cfg.Policy,
		Now:     now,
	})
}

func (e *Engine) Movers() *mover.Movers {
	return &mover.Movers{Radarr: e.Radarr, Sonarr: e.Sonarr, History: e.History}
}

// JellyfinClient returns a client for the configured Jellyfin instance, or
// nil if Jellyfin rescan-on-move isn't configured.
func (e *Engine) JellyfinClient() *jellyfin.Client {
	if !e.Cfg.JellyfinEnabled() {
		return nil
	}
	return jellyfin.NewClient(e.Cfg.Jellyfin.URL, e.Cfg.Jellyfin.APIKey)
}
