package engine

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/diskusage"
	"github.com/vocoder/coldarr/internal/importcache"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/qbit"
	"github.com/vocoder/coldarr/internal/torrents"
)

// ErrQbitNotConnected is returned by every downloads operation when there's
// no qBittorrent connection to work through.
var ErrQbitNotConnected = errors.New("qBittorrent isn't connected - set it up under Settings > Connections")

// DownloadsEnabled reports whether Coldarr tiers downloads at all: a
// qBittorrent connection, and at least one downloads tier to manage.
func (e *Engine) DownloadsEnabled() bool {
	return e.Qbit != nil && len(model.TiersIn(e.Cfg.Tiers, model.DomainDownloads)) > 0
}

// DownloadsInventory is a snapshot of qBittorrent's downloads, classified
// (see internal/torrents), alongside the state of every tier path.
type DownloadsInventory struct {
	// Tiers is the downloads tiers, in their configured order.
	Tiers []model.Tier
	// PathStatus covers every configured tier path, library ones too, so
	// a downloads path sharing a disk with a library path is planned as
	// the one capacity pool it is.
	PathStatus map[string]PathStatus
	Downloads  []*torrents.Download
	// Unseen marks downloads tier paths where qBittorrent lists finished
	// downloads but Coldarr can't see any of them - most likely mounted
	// somewhere else in Coldarr than in qBittorrent. Nothing moves to or
	// from such a path.
	Unseen map[string]bool
	// Reserved is, per usable path, how many more bytes downloads still in
	// progress will write to its disk.
	Reserved map[string]int64
	Warnings []string
}

// UsableUsage returns usage for only the paths that passed their checks.
func (inv *DownloadsInventory) UsableUsage() map[string]diskusage.Usage {
	return (&Inventory{PathStatus: inv.PathStatus}).UsableUsage()
}

// VolumeOf returns each path's device ID, where known.
func (inv *DownloadsInventory) VolumeOf() map[string]uint64 {
	return (&Inventory{PathStatus: inv.PathStatus}).VolumeOf()
}

// Count returns how many downloads are in each state - the managed ones
// only, since an ignored download is none of Coldarr's business.
func (inv *DownloadsInventory) Count() map[torrents.State]int {
	counts := map[torrents.State]int{}
	for _, d := range inv.Downloads {
		if d.TierPath != "" {
			counts[d.State]++
		}
	}
	return counts
}

// BuildDownloadsInventory checks every tier path, lists qBittorrent's
// downloads, joins them against every *arr app's queue, and classifies
// them. It performs no writes, besides remembering confirmed imports.
//
// liveImportChecks asks the *arr apps about any import not yet confirmed -
// one request per download that's otherwise ready to move, and never again
// once confirmed (see internal/importcache). Plans and applies pass true;
// a page that only counts things passes false and makes do with what's
// already known.
func (e *Engine) BuildDownloadsInventory(now time.Time, liveImportChecks bool) (*DownloadsInventory, error) {
	if e.Qbit == nil {
		return nil, ErrQbitNotConnected
	}
	inv := &DownloadsInventory{
		Tiers:      model.TiersIn(e.Cfg.Tiers, model.DomainDownloads),
		PathStatus: e.checkPaths(),
		Unseen:     map[string]bool{},
	}

	var (
		list              []qbit.Torrent
		listErr, queueErr error
		queued            map[string]string
		wg                sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); list, listErr = e.Qbit.Torrents() }()
	go func() { defer wg.Done(); queued, queueErr = e.arrQueues() }()
	wg.Wait()
	if listErr != nil {
		return nil, fmt.Errorf("listing qBittorrent's downloads: %w", listErr)
	}
	// Fatal, not a warning: without the queues there's no telling which
	// downloads are mid-import, and moving one of those breaks the import.
	if queueErr != nil {
		return nil, queueErr
	}

	fs := &cachedFS{}
	in := torrents.Input{
		Torrents: list,
		Tiers:    e.Cfg.Tiers,
		Config:   e.Cfg.Torrents,
		Queued:   queued,
		Imports:  map[torrents.ImportKey]torrents.ImportResult{},
		Connected: map[string]bool{
			"radarr": e.Radarr != nil,
			"sonarr": e.Sonarr != nil,
			"lidarr": e.Lidarr != nil,
		},
		FS:  fs,
		Now: now,
	}
	downloads := torrents.Classify(in)
	if needed := torrents.ImportChecksNeeded(downloads); len(needed) > 0 {
		in.Imports = e.importResults(needed, list, liveImportChecks, now)
		downloads = torrents.Classify(in)
	}
	inv.Downloads = downloads
	inv.Reserved = reservations(list, inv.PathStatus)
	inv.Warnings = append(inv.Warnings, inv.findUnseen(fs)...)
	return inv, nil
}

// arrQueues returns every *arr app's queued download IDs, mapped to the
// app - fetched concurrently, since each is a separate backend.
func (e *Engine) arrQueues() (map[string]string, error) {
	type source struct {
		app   string
		fetch func() (map[string]bool, error)
	}
	var sources []source
	if e.Radarr != nil {
		sources = append(sources, source{"radarr", e.Radarr.QueueDownloadIDs})
	}
	if e.Sonarr != nil {
		sources = append(sources, source{"sonarr", e.Sonarr.QueueDownloadIDs})
	}
	if e.Lidarr != nil {
		sources = append(sources, source{"lidarr", e.Lidarr.QueueDownloadIDs})
	}

	ids := make([]map[string]bool, len(sources))
	errs := make([]error, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i], errs[i] = src.fetch()
		}()
	}
	wg.Wait()

	queued := map[string]string{}
	for i, src := range sources {
		if errs[i] != nil {
			return nil, fmt.Errorf("reading %s's queue: %w", src.app, errs[i])
		}
		for id := range ids[i] {
			queued[id] = src.app
		}
	}
	return queued, nil
}

// importCheckConcurrency bounds how many import checks run at once, so a
// first run with a backlog of settled downloads doesn't fire hundreds of
// history lookups at Radarr in the same instant.
const importCheckConcurrency = 4

// importResults answers the import checks: confirmations already on file
// come from the cache, and - when live - the rest are asked of each app,
// with any new confirmations remembered. Entries for downloads
// qBittorrent no longer has are pruned along the way.
func (e *Engine) importResults(keys []torrents.ImportKey, list []qbit.Torrent, live bool, now time.Time) map[torrents.ImportKey]torrents.ImportResult {
	results := map[torrents.ImportKey]torrents.ImportResult{}
	var ask []torrents.ImportKey
	for _, k := range keys {
		switch {
		case e.Imports.Imported(k.App, k.Hash):
			results[k] = torrents.ImportResult{Imported: true}
		case live:
			ask = append(ask, k)
		}
	}
	if len(ask) == 0 {
		return results
	}

	answers := make([]torrents.ImportResult, len(ask))
	sem := make(chan struct{}, importCheckConcurrency)
	var wg sync.WaitGroup
	for i, k := range ask {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			imported, err := e.imported(k)
			if err != nil {
				answers[i] = torrents.ImportResult{Err: err.Error()}
				return
			}
			answers[i] = torrents.ImportResult{Imported: imported}
		}()
	}
	wg.Wait()

	var confirmed []importcache.Confirmation
	for i, k := range ask {
		results[k] = answers[i]
		if answers[i].Imported {
			confirmed = append(confirmed, importcache.Confirmation{App: k.App, Hash: k.Hash})
		}
	}
	keep := make(map[string]bool, len(list))
	for _, t := range list {
		keep[strings.ToUpper(t.Hash)] = true
	}
	// Losing the cache only costs asking again next time.
	if err := e.Imports.Record(confirmed, keep, now); err != nil {
		log.Printf("engine: remembering confirmed imports failed, they'll be checked again: %v", err)
	}
	return results
}

func (e *Engine) imported(k torrents.ImportKey) (bool, error) {
	switch k.App {
	case "radarr":
		if e.Radarr != nil {
			return e.Radarr.Imported(k.Hash)
		}
	case "sonarr":
		if e.Sonarr != nil {
			return e.Sonarr.Imported(k.Hash)
		}
	case "lidarr":
		if e.Lidarr != nil {
			return e.Lidarr.Imported(k.Hash)
		}
	}
	return false, fmt.Errorf("%s isn't connected", k.App)
}

// unseenSample is how many finished downloads per tier path are checked
// for being visible to Coldarr - enough to tell a mismatched mount from a
// download whose files were deleted behind qBittorrent's back.
const unseenSample = 5

// findUnseen checks, for each usable downloads tier path, whether Coldarr
// can see the finished downloads qBittorrent says are there. Coldarr and
// qBittorrent are meant to see the same paths; if a path was mounted from
// a different drive in one of them, everything qBittorrent lists on it is
// invisible here - and moving into it would put downloads somewhere
// Coldarr isn't measuring. Only a path where every sampled download is
// missing counts, since one missing download is more likely deleted than
// mis-mounted.
func (inv *DownloadsInventory) findUnseen(fs torrents.FS) []string {
	sampled := map[string][]*torrents.Download{}
	for _, d := range inv.Downloads {
		if d.TierPath == "" || !d.Complete() || !qbit.Seeding(d.Torrent.State) || len(sampled[d.TierPath]) >= unseenSample {
			continue
		}
		sampled[d.TierPath] = append(sampled[d.TierPath], d)
	}

	paths := make([]string, 0, len(sampled))
	for p := range sampled {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var warnings []string
	for _, path := range paths {
		if status, ok := inv.PathStatus[path]; !ok || status.Err != nil {
			continue
		}
		seen := false
		for _, d := range sampled[path] {
			if exists, err := fs.Exists(d.ContentPath); err != nil || exists {
				seen = true
				break
			}
		}
		if !seen {
			inv.Unseen[path] = true
			warnings = append(warnings, fmt.Sprintf("qBittorrent lists finished downloads under %s, but Coldarr can't see any of them there - is %s mounted into Coldarr from the same drive, at the same path, as in qBittorrent? Nothing moves to or from it until it is.", path, path))
		}
	}
	return warnings
}

// cachedFS answers Exists from the real filesystem, remembering answers -
// classification runs twice when import checks are needed, and shouldn't
// stat every download twice.
type cachedFS struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (f *cachedFS) Exists(path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if exists, ok := f.seen[path]; ok {
		return exists, nil
	}
	_, err := os.Lstat(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
	default:
		return false, err
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	f.seen[path] = err == nil
	return err == nil, nil
}

// TorrentMovesInFlight is ArrMovesInFlight's counterpart for qBittorrent:
// whether it is still moving, hash-checking or allocating any download.
// Those carry on inside qBittorrent whether or not the Coldarr process
// that asked for them is still alive, and disk numbers read meanwhile are
// unreliable - for library tiers as much as downloads ones, when they
// share a disk. An error is returned as an error, never treated as idle.
func (e *Engine) TorrentMovesInFlight() (bool, string, error) {
	if e.Qbit == nil {
		return false, "", nil
	}
	list, err := e.Qbit.Torrents()
	if err != nil {
		return false, "", fmt.Errorf("checking qBittorrent for in-flight moves: %w", err)
	}
	var busy []string
	for _, t := range list {
		if qbit.InFlight(t.State) {
			busy = append(busy, t.Name)
		}
	}
	if len(busy) == 0 {
		return false, "", nil
	}
	sort.Strings(busy)
	examples := busy
	if len(examples) > 3 {
		examples = append(examples[:3:3], "...")
	}
	return true, fmt.Sprintf("qBittorrent is still moving or checking %d download(s) (%s) - this carries on even across Coldarr restarts. Wait for it to finish before planning or applying anything.", len(busy), strings.Join(examples, ", ")), nil
}

// MovesInFlight checks every app that may still be moving data -
// Radarr/Sonarr and qBittorrent - and is what every plan and apply gates
// on, library and downloads alike: a move in one domain can be filling the
// disk the other is about to measure.
func (e *Engine) MovesInFlight() (bool, string, error) {
	arrBusy, arrDetail, err := e.ArrMovesInFlight()
	if err != nil {
		return false, "", err
	}
	qbitBusy, qbitDetail, err := e.TorrentMovesInFlight()
	if err != nil {
		return false, "", err
	}
	var details []string
	if arrBusy {
		details = append(details, arrDetail)
	}
	if qbitBusy {
		details = append(details, qbitDetail)
	}
	return len(details) > 0, strings.Join(details, " "), nil
}

// reservations works out, per tier path, how many more bytes downloads
// still in progress will write to its disk: qBittorrent's amount left, for
// every incomplete download that isn't paused or errored. Neither planner
// can see that space being claimed until it already has been, so it's held
// back up front instead. A download is attributed to a disk by where its
// data is (or, if that doesn't exist yet, where it's going); one Coldarr
// can't place on any tier's disk is no concern of the plan.
func reservations(downloads []qbit.Torrent, status map[string]PathStatus) map[string]int64 {
	byDevice := map[uint64]int64{}
	byPath := map[string]int64{}
	for _, t := range downloads {
		if t.Complete() || t.AmountLeft <= 0 || qbit.Paused(t.State) || qbit.Errored(t.State) {
			continue
		}
		if dev, ok := downloadDevice(t); ok {
			byDevice[dev] += t.AmountLeft
			continue
		}
		if path := tierPathFor(t.SavePath, status); path != "" {
			if st := status[path]; st.DeviceIDKnown {
				byDevice[st.DeviceID] += t.AmountLeft
			} else {
				byPath[path] += t.AmountLeft
			}
		}
	}

	out := map[string]int64{}
	for path, st := range status {
		if st.Err != nil {
			continue
		}
		n := byPath[path]
		if st.DeviceIDKnown {
			n += byDevice[st.DeviceID]
		}
		if n > 0 {
			out[path] = n
		}
	}
	return out
}

// downloadDevice finds the disk a download is writing to: its content if
// that exists yet, else the folder it's going into.
func downloadDevice(t qbit.Torrent) (uint64, bool) {
	for _, p := range []string{t.ContentPath, filepath.Dir(t.ContentPath), t.SavePath} {
		if p == "" || p == "." {
			continue
		}
		if dev, err := diskusage.DeviceID(p); err == nil {
			return dev, true
		}
	}
	return 0, false
}

// tierPathFor returns the deepest configured tier path containing path.
func tierPathFor(path string, status map[string]PathStatus) string {
	clean := filepath.Clean(path)
	best := ""
	for p := range status {
		root := filepath.Clean(p)
		if (clean == root || strings.HasPrefix(clean, root+string(filepath.Separator))) && len(root) > len(best) {
			best = p
		}
	}
	return best
}

// applyReservations returns usage with each path's reservation counted as
// already used.
func applyReservations(usage map[string]diskusage.Usage, reserved map[string]int64) map[string]diskusage.Usage {
	if len(reserved) == 0 {
		return usage
	}
	out := make(map[string]diskusage.Usage, len(usage))
	for path, u := range usage {
		if n := reserved[path]; n > 0 {
			used := u.UsedBytes + uint64(n)
			free := uint64(0)
			if uint64(n) < u.FreeBytes {
				free = u.FreeBytes - uint64(n)
			}
			u.UsedBytes, u.FreeBytes = used, free
			u.UsedPercent = diskusage.PercentUsed(used, free)
		}
		out[path] = u
	}
	return out
}

// reservationNotes describes each reservation once per tier path, in
// configured order, so a plan's projected usage can be read in the light of
// it.
func reservationNotes(reserved map[string]int64, tiers []model.Tier) []string {
	var notes []string
	for _, tier := range tiers {
		for _, path := range tier.Paths {
			if n := reserved[path]; n > 0 {
				notes = append(notes, fmt.Sprintf("%.1f GB is held back on %s for downloads still in progress - projected usage counts it as used", float64(n)/(1<<30), path))
			}
		}
	}
	return notes
}

// Wedged returns, for each seed storage path that unmanaged content alone
// fills past its max - downloads Coldarr leaves alone, and anything that
// isn't a download at all - how much of it there is. Nothing Coldarr can
// do will make room there: the content it manages could all expire and the
// path would still be over its ceiling, so no amount of waiting helps.
// Unusable paths aren't judged.
func (inv *DownloadsInventory) Wedged() map[string]int64 {
	managed := map[string]int64{}
	for _, d := range inv.Downloads {
		if d.TierPath != "" && d.State != torrents.Ignored {
			managed[d.TierPath] += d.Size
		}
	}
	out := map[string]int64{}
	for _, tier := range inv.Tiers {
		if tier.Role != model.RoleCold {
			continue
		}
		for _, path := range tier.Paths {
			status, ok := inv.PathStatus[path]
			if !ok || status.Err != nil {
				continue
			}
			u := status.Usage
			unmanaged := int64(u.UsedBytes) - managed[filepath.Clean(path)] //nolint:gosec // disk usage byte counts never approach int64 overflow range
			if unmanaged <= 0 {
				continue
			}
			if diskusage.PercentUsed(uint64(unmanaged), u.UsedBytes+u.FreeBytes-uint64(unmanaged)) >= tier.MaxUsedPercent {
				out[path] = unmanaged
			}
		}
	}
	return out
}
