// Package engine wires together config, the Arr API clients, disk usage
// checks, scoring, and planning into the handful of operations the CLI
// commands need: build an inventory, build a plan from it, and hand off
// execution to the mover.
package engine

import (
	"errors"
	"fmt"
	"maps"
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

// CheckStorage re-checks every configured tier path (see
// diskusage.CheckPath) and refuses if any fails. A missing or dead drive
// doesn't only affect moves to or from it: its mountpoint directory is
// still there, reporting the system disk's free space, and a plan built
// around an absent drive is built on a false picture of the library.
// So one unavailable path blocks every move until it's back - every apply
// entry point checks this before building a plan, and the mover checks it
// again before each move, since a drive can drop out mid-run.
func (e *Engine) CheckStorage() error {
	var problems []string
	for _, tier := range e.Cfg.Tiers {
		for _, path := range tier.Paths {
			if err := diskusage.CheckPath(path, tier.RequireMount); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", tier.Name, err))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("storage unavailable, refusing to move anything until every tier path checks healthy - %s", strings.Join(problems, "; "))
	}
	return nil
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
	// inventory that the operator should see but that shouldn't block
	// report/plan/apply. Failure to fetch enabled Jellyfin favorites is
	// deliberately fatal instead: without that snapshot, Coldarr cannot
	// know which items must be kept on hot storage.
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
// every enabled Arr app, snapshots enabled Jellyfin favorites, and scores
// each item. It performs no writes. If the Jellyfin snapshot fails, the
// entire build fails closed so no plan or apply can proceed without favorite
// protection.
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
		favoritesErr         error
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
				favoritesErr = err
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
	if favoritesErr != nil {
		return nil, fmt.Errorf("fetching favorites from Jellyfin: %w; refusing to continue because favorite protection could not be established", favoritesErr)
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
// or slower than the defaults assume. followUp, from StartJellyfinFollowUp,
// is handed each move as it lands; nil when Jellyfin isn't configured.
func (e *Engine) Movers(followUp *JellyfinFollowUp) *mover.Movers {
	m := &mover.Movers{
		Radarr:              e.Radarr,
		Sonarr:              e.Sonarr,
		History:             e.History,
		CheckStorage:        e.CheckStorage,
		SettleCheckInterval: envDuration("COLDARR_SETTLE_CHECK_INTERVAL"),
		SettleStableChecks:  envInt("COLDARR_SETTLE_STABLE_CHECKS"),
		SettleMaxWait:       envDuration("COLDARR_SETTLE_MAX_WAIT"),
	}
	// Assigned through an explicit nil check: a typed nil
	// *JellyfinFollowUp stored in the interface field would leave it
	// non-nil and panic on first use.
	if followUp != nil {
		m.Reporter = followUp
	}
	return m
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
	c := jellyfin.NewClient(e.jellyfinConn.URL, e.jellyfinConn.APIKey)
	// How long Jellyfin takes to surface a moved item depends on its own
	// LibraryMonitorDelay and on how long re-validating a library root
	// takes, neither of which Coldarr can see - so both bounds are
	// overridable for libraries where the defaults don't fit.
	if d := envDuration("COLDARR_JELLYFIN_RESOLVE_INTERVAL"); d > 0 {
		c.ResolvePollInterval = d
	}
	if d := envDuration("COLDARR_JELLYFIN_RESOLVE_TIMEOUT"); d > 0 {
		c.ResolveTimeout = d
	}
	return c
}

// JellyfinFollowUp is one apply run's Jellyfin work, done item by item as
// each move lands rather than once the run is over. It holds the date
// added of everything the plan moves, taken before the first move, and is
// the mover's MoveReporter: when a move is confirmed landed, it reports the
// paths to Jellyfin, then in the background waits for Jellyfin to index the
// item at its new path, puts the item's dates back and refreshes it, while
// the next move runs. If Coldarr stops part way through a run, only items
// still waiting on Jellyfin miss that.
type JellyfinFollowUp struct {
	jf    *jellyfin.Client
	added map[string]jellyfin.AddedDates

	wg sync.WaitGroup
	mu sync.Mutex
	// outcome is each finished follow-up's result, by landed path.
	outcome map[string]error
}

// StartJellyfinFollowUp records the date Jellyfin shows as added for every
// movie and episode the plan is about to move, and returns the follow-up
// to hand to Movers and NotifyJellyfinMoved. Nil when Jellyfin isn't
// configured.
//
// Call it before Apply. Jellyfin deletes each old item, and the old date
// with it, as soon as it rescans the folder that item's move vacated.
//
// An error means some dates couldn't be read, and those items will show
// up under Recently Added again. That's no reason to hold the run back, so
// the follow-up comes back with whatever was read and callers log the
// error.
func (e *Engine) StartJellyfinFollowUp(plan *planner.Plan) (*JellyfinFollowUp, error) {
	jf := e.JellyfinClient()
	if jf == nil {
		return nil, nil
	}
	followUp := &JellyfinFollowUp{jf: jf, outcome: map[string]error{}}
	if plan == nil || len(plan.Entries) == 0 {
		return followUp, nil
	}

	folders := make([]string, 0, len(plan.Entries))
	for _, entry := range plan.Entries {
		folders = append(folders, entry.Item.Path)
	}
	added, err := jf.SnapshotAddedDates(folders)
	followUp.added = added
	return followUp, err
}

// ReportMoved implements mover.MoveReporter. A failed report is returned,
// which leaves the item for NotifyJellyfinMoved to report and follow up
// after the run; a successful one starts the item's follow-up.
func (f *JellyfinFollowUp) ReportMoved(oldPath, newPath string) error {
	item := jellyfin.MovedItem{
		Title:      filepath.Base(newPath),
		OldPath:    oldPath,
		NewPath:    newPath,
		AddedDates: f.datesFor(oldPath),
	}
	if err := f.jf.ReportMoved([]jellyfin.MovedItem{item}); err != nil {
		return err
	}

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		err := f.jf.ResolveAndRefresh([]jellyfin.MovedItem{item})
		f.mu.Lock()
		f.outcome[filepath.Clean(newPath)] = err
		f.mu.Unlock()
	}()
	return nil
}

// wait blocks until every follow-up started so far has finished, and
// returns their results by landed path.
func (f *JellyfinFollowUp) wait() map[string]error {
	if f == nil {
		return nil
	}
	f.wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.outcome)
}

// datesFor returns the dates recorded for the folder an item moved from.
func (f *JellyfinFollowUp) datesFor(oldPath string) jellyfin.AddedDates {
	if f == nil {
		return nil
	}
	return f.added[filepath.Clean(oldPath)]
}

// NotifyJellyfinMoved finishes the Jellyfin side of a completed apply run.
// A no-op when Jellyfin isn't configured.
//
// Most of it is already done: followUp has been putting back each moved
// item's date added and refreshing its artwork as each move landed. This
// waits for the last of those, then does the same in one batch for any
// moved item that never got a follow-up - its report to Jellyfin failed,
// or no follow-up was passed.
//
// Per-item targeting is the point: a whole-library scan runs in Jellyfin's
// "Default" refresh mode, which only fills in artwork it thinks is
// missing and so cannot displace image records still pointing into the
// tier an item just left (see jellyfin.FullRefreshOptions). The scan
// survives only as the fallback for items whose new path couldn't be
// resolved - better than nothing, but it is not the fix.
func (e *Engine) NotifyJellyfinMoved(moved []mover.EntryProgress, followUp *JellyfinFollowUp) error {
	jf := e.JellyfinClient()
	if jf == nil {
		return nil
	}

	followedUp := followUp.wait()

	items := make([]jellyfin.MovedItem, 0, len(moved))
	var unreported []jellyfin.MovedItem
	var unresolved []string
	var failed []error
	for _, m := range moved {
		// A confirmed move always records where it landed; anything else
		// can't be targeted by path and only the library scan can help.
		if m.LandedPath == "" {
			unresolved = append(unresolved, m.Entry.Item.Title)
			continue
		}
		if err, ok := followedUp[filepath.Clean(m.LandedPath)]; ok {
			if err != nil {
				failed = append(failed, err)
			}
			continue
		}
		item := jellyfin.MovedItem{
			Title:      m.Entry.Item.Title,
			OldPath:    m.Entry.Item.Path,
			NewPath:    m.LandedPath,
			AddedDates: followUp.datesFor(m.Entry.Item.Path),
		}
		items = append(items, item)
		if !m.Reported {
			unreported = append(unreported, item)
		}
	}

	// Strictly the items the run couldn't report as they landed - Jellyfin
	// wasn't configured when the move ran, or the report failed. Reporting
	// the rest a second time would restart Jellyfin's debounce on paths it
	// already has queued, pushing back the very rescans this is waiting on.
	if len(unreported) > 0 {
		// Discarded deliberately, not overlooked: resolving below is what
		// actually establishes the item exists, so a missed hint costs
		// only a head start, and the client has already logged the failing
		// request itself. Surfacing it here would turn a run that then
		// refreshed everything fine into a reported failure.
		_ = jf.ReportMoved(unreported)
	}

	notifyErr := errors.Join(append(failed, jf.ResolveAndRefresh(items))...)
	if notifyErr == nil && len(unresolved) == 0 {
		return nil
	}

	if len(unresolved) > 0 {
		notifyErr = errors.Join(notifyErr, fmt.Errorf("no landed path recorded for %s", strings.Join(unresolved, ", ")))
	}
	if err := jf.RefreshLibrary(); err != nil {
		return fmt.Errorf("%w; whole-library fallback scan also failed: %w", notifyErr, err)
	}
	return fmt.Errorf("%w; fell back to a whole-library scan", notifyErr)
}
