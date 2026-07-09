// Package engine wires together config, the Arr API clients, disk usage
// checks, scoring, and planning into the handful of operations the CLI
// commands need: build an inventory, build a plan from it, and hand off
// execution to the mover.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/arrapi"
	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/cutoffcache"
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
	Cfg     *config.Config
	Radarr  *arrapi.RadarrClient
	Sonarr  *arrapi.SonarrClient
	History *history.Store
	// CutoffCache holds which items have an unmet quality-profile cutoff
	// (see internal/cutoffcache) - read in BuildInventory, but never
	// refreshed there. Refreshing means live-fetching Radarr/Sonarr's
	// wanted/cutoff list, which is too slow on real libraries to do on
	// every plan/dashboard page load; it only ever happens from the
	// "Scan Quality Cutoffs" scheduled task or its manual "Scan now"
	// trigger. A cache that's never been refreshed just means every item
	// is treated as cutoff-met, same as before this feature existed.
	CutoffCache  *cutoffcache.Store
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

	cutoffCache, err := cutoffcache.Load(cutoffCachePath(cfg.History.Path))
	if err != nil {
		return nil, err
	}
	e.CutoffCache = cutoffCache

	return e, nil
}

// cutoffCachePath derives the quality-cutoff cache's location alongside
// the history file - not a separately configurable path, since it's
// purely an internal cache rather than something an operator needs to
// point elsewhere (same reasoning as internal/webui's linkCachePath).
func cutoffCachePath(historyPath string) string {
	return filepath.Join(filepath.Dir(historyPath), "coldarr-cutoffcache.json")
}

// ArrMovesInFlight reports whether Radarr or Sonarr is still executing (or
// has queued) any move command - regardless of who requested it or
// whether the requesting process is even alive anymore. Coldarr's own
// apply lock is an flock that the kernel releases the moment Coldarr's
// process exits, but move commands already handed to Radarr/Sonarr keep
// physically copying files entirely on their own; after a Coldarr crash
// or restart, this is the ONLY signal that those writes are still
// happening. Every apply entry point must refuse to start - and any
// freshly-built plan should be distrusted - while this reports true:
// disk-usage numbers taken mid-move are garbage, and starting new moves
// on top of in-flight ones is how destination drives get overfilled. An
// error from either app is returned as an error, not treated as idle -
// "can't tell" must fail toward not moving anything.
func (e *Engine) ArrMovesInFlight() (bool, string, error) {
	total := 0
	var parts []string

	if e.Radarr != nil {
		n, err := e.Radarr.ActiveMoveCommands()
		if err != nil {
			return false, "", fmt.Errorf("checking radarr for in-flight moves: %w", err)
		}
		if n > 0 {
			parts = append(parts, fmt.Sprintf("Radarr is still executing %d move command(s)", n))
		}
		total += n
	}
	if e.Sonarr != nil {
		n, err := e.Sonarr.ActiveMoveCommands()
		if err != nil {
			return false, "", fmt.Errorf("checking sonarr for in-flight moves: %w", err)
		}
		if n > 0 {
			parts = append(parts, fmt.Sprintf("Sonarr is still executing %d move command(s)", n))
		}
		total += n
	}

	if total == 0 {
		return false, "", nil
	}
	return true, strings.Join(parts, "; ") + " - these keep running even across Coldarr restarts. Wait for them to finish before planning or applying anything.", nil
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

// ColdTierPaths returns PathStatus for every path belonging to a
// cold-role tier - used by the scheduled "Rescan Cold Storage" task,
// which only reports on cold storage, not the whole inventory.
func (inv *Inventory) ColdTierPaths() []PathStatus {
	var out []PathStatus
	for _, status := range inv.PathStatus {
		if status.Tier.Role == model.RoleCold {
			out = append(out, status)
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

	// Radarr, Sonarr, and Jellyfin are independent backends, so fetch all
	// three concurrently - sequentially they'd add up to three backends'
	// worth of network latency on every plan/dashboard page load.
	var (
		movies, series       []model.MediaItem
		moviesErr, seriesErr error
		favorites            map[string]bool
		favWarning           string
	)

	var wg sync.WaitGroup

	if e.Radarr != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			movies, moviesErr = e.Radarr.FetchMovies()
		}()
	}

	if e.Sonarr != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			series, seriesErr = e.Sonarr.FetchSeries()
		}()
	}

	if jf := e.JellyfinClient(); jf != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths, err := jf.FavoritePaths()
			if err != nil {
				favWarning = fmt.Sprintf("could not fetch Jellyfin favorites, favorited items are NOT protected this run: %v", err)
				return
			}
			favorites = paths
		}()
	}

	wg.Wait()

	if moviesErr != nil {
		return nil, fmt.Errorf("fetching movies from radarr: %w", moviesErr)
	}
	if seriesErr != nil {
		return nil, fmt.Errorf("fetching series from sonarr: %w", seriesErr)
	}
	if favWarning != "" {
		inv.Warnings = append(inv.Warnings, favWarning)
	}

	items := make([]model.MediaItem, 0, len(movies)+len(series))
	items = append(items, movies...)
	items = append(items, series...)

	cutoffSnap := e.CutoffCache.Get()
	if cutoffSnap.RefreshedAt.IsZero() && (e.Radarr != nil || e.Sonarr != nil) {
		inv.Warnings = append(inv.Warnings, `Quality-cutoff status has never been scanned - items whose file doesn't meet its quality profile's cutoff won't be kept on hot storage for that reason yet. Enable or manually run "Scan Quality Cutoffs" under Settings > Scheduler.`)
	}

	for _, it := range items {
		if favorites[filepath.Clean(it.Path)] {
			it.JellyfinFavorite = true
		}
		switch it.ArrApp {
		case "radarr":
			it.QualityCutoffNotMet = cutoffSnap.RadarrUnmetIDs[it.ID]
		case "sonarr":
			it.QualityCutoffNotMet = cutoffSnap.SonarrUnmetIDs[it.ID]
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

// Movers returns a mover.Movers configured with production settle-timing
// defaults, unless overridden via COLDARR_SETTLE_CHECK_INTERVAL /
// COLDARR_SETTLE_STABLE_CHECKS / COLDARR_SETTLE_MAX_WAIT (Go duration
// strings like "5s", "6h") - useful for storage that settles much faster
// or slower than the defaults assume.
func (e *Engine) Movers() *mover.Movers {
	return &mover.Movers{
		Radarr:              e.Radarr,
		Sonarr:              e.Sonarr,
		History:             e.History,
		SettleCheckInterval: envDuration("COLDARR_SETTLE_CHECK_INTERVAL"),
		SettleStableChecks:  envInt("COLDARR_SETTLE_STABLE_CHECKS"),
		SettleMaxWait:       envDuration("COLDARR_SETTLE_MAX_WAIT"),
	}
}

func envDuration(name string) time.Duration {
	v, ok := os.LookupEnv(name)
	if !ok {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}

func envInt(name string) int {
	v, ok := os.LookupEnv(name)
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// JellyfinClient returns a client for the configured Jellyfin instance, or
// nil if Jellyfin rescan-on-move isn't configured.
func (e *Engine) JellyfinClient() *jellyfin.Client {
	if !e.jellyfinOK {
		return nil
	}
	return jellyfin.NewClient(e.jellyfinConn.URL, e.jellyfinConn.APIKey)
}
