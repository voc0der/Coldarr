// Package torrents decides what Coldarr does with each qBittorrent
// download: which ones it manages at all, which have settled long enough
// to move from the download drive to seed storage, and what holds the rest
// back. Like internal/scoring it makes decisions and touches nothing -
// qBittorrent does every move - but there's no score: whether a download
// moves is a matter of rules, not ranking.
//
// A download's state comes from where qBittorrent says it is - which tier
// its save path sits under - never from its labels. The seed label is
// applied only after a move has landed, as a courtesy to whoever looks at
// qBittorrent, so a move that fails part way can never leave a download
// labeled as archived while its data is still on the download drive.
package torrents

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/qbit"
)

// State is where a download stands in the download-tiering lifecycle.
type State string

const (
	// Ignored: Coldarr never touches it - it's outside every downloads
	// tier, or none of its labels say to move it. Its bytes still count
	// toward each drive's usage, which comes from the filesystem.
	Ignored State = "ignored"
	// Active: still downloading, being moved or checked by qBittorrent,
	// errored, or in an *arr app's queue. Never moved while so.
	Active State = "active"
	// Waiting: finished, on the download drive, sitting out the settle
	// time before it moves.
	Waiting State = "waiting"
	// Ready: settled, and nothing stands in the way of moving it.
	Ready State = "ready"
	// Held: settled, but something stands in the way of moving it - its
	// Reason says what.
	Held State = "held"
	// Parked: on seed storage.
	Parked State = "parked"
)

// FS is what classification needs to know about the disk, as Coldarr
// sees it - which isn't necessarily how qBittorrent does, if someone
// mounted a drive somewhere else.
type FS interface {
	Exists(path string) (bool, error)
}

// ImportKey names one import check: whether App has imported the download
// with this upper-cased hash, which is how the *arr apps record it.
type ImportKey struct {
	App  string
	Hash string
}

// ImportResult is the answer to one import check.
type ImportResult struct {
	Imported bool
	// Err is set when the app couldn't be asked; Imported is then false.
	Err string
}

// Input is everything Classify needs.
type Input struct {
	Torrents []qbit.Torrent
	// Tiers is every configured tier; only downloads tiers are used.
	Tiers  []model.Tier
	Config config.TorrentsConfig
	// Queued maps the upper-cased hash of every download an *arr app
	// still has queued - still fetching, or waiting to be imported - to
	// that app.
	Queued map[string]string
	// Imports holds the answer to each import check that has been run.
	// A download that needs one that hasn't is held, and listed by
	// ImportChecksNeeded so the caller can run it and classify again.
	Imports map[ImportKey]ImportResult
	// Connected says which *arr apps Coldarr can ask about imports.
	Connected map[string]bool
	// Trackers says which trackers each download announces to; nil means
	// none are known yet, so no seeding requirement can be.
	Trackers Trackers
	FS       FS
	Now      time.Time
}

// Download is one qBittorrent torrent with what Coldarr makes of it. Its
// State is Coldarr's; qBittorrent's own state is Torrent.State.
type Download struct {
	qbit.Torrent
	// Tier and TierPath locate the download: the downloads tier, and the
	// configured path its save path sits under. Both are zero for a
	// download outside every downloads tier.
	Tier     model.Tier
	TierPath string
	State    State
	// Reason says why the download is in its state, for the Downloads
	// page. Empty where the state says it all.
	Reason string
	// Group is the download's content path, cleaned. Downloads sharing
	// one are cross-seeds of the same files, and only ever move together.
	Group string
	// ImportedBy lists the apps that import downloads with this one's
	// labels - each must have imported it before it moves.
	ImportedBy []string
	// SettledAt is when the download will have sat finished for the
	// settle time; zero when qBittorrent doesn't say when it finished.
	SettledAt time.Time
	// Relabel marks a download already on seed storage that still has a
	// download label rather than the seed label - left by a move whose
	// relabel never ran (Coldarr stopped in between), or by a download
	// that landed straight on seed storage. Set only once it's clear to
	// relabel: the same import and Automatic Torrent Management checks a
	// move gets.
	Relabel bool
	// DisableAutoTMM marks a download whose Automatic Torrent Management
	// Coldarr will switch off before moving or relabeling it - only ever
	// set when the settings allow that.
	DisableAutoTMM bool
	// Seeding is how the download stands against its trackers' seeding
	// requirement - worked out for every finished download Coldarr
	// manages, wherever it is.
	Seeding Seeding
	// ImportsConfirmed is set once every app in ImportedBy has confirmed
	// importing the download - and from the start when there are none.
	// It's only worked out where it matters: before a move or relabel,
	// and for a download whose seeding is done.
	ImportsConfirmed bool
	// pendingImports are the import checks the download is waiting on.
	pendingImports []ImportKey
	// importReason memoizes importsReason.
	importReason  string
	importChecked bool
}

// Managed reports whether the download is Coldarr's to move - on a
// downloads tier, with a label that says to move it.
func (d *Download) Managed() bool {
	return d.State != Ignored
}

// Classify works out the state of every download, cross-seeds included:
// downloads sharing files only ever move together, so one that's held
// holds back the rest. The result is sorted by name.
func Classify(in Input) []*Download {
	tiers := model.TiersIn(in.Tiers, model.DomainDownloads)
	out := make([]*Download, 0, len(in.Torrents))
	for _, t := range in.Torrents {
		out = append(out, classifyOne(in, tiers, t))
	}
	holdGroups(out)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return out[i].Hash < out[j].Hash
	})
	return out
}

// ImportChecksNeeded lists every import check a download is waiting on -
// once each, in a stable order.
func ImportChecksNeeded(downloads []*Download) []ImportKey {
	seen := map[ImportKey]bool{}
	var keys []ImportKey
	for _, d := range downloads {
		for _, k := range d.pendingImports {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].App != keys[j].App {
			return keys[i].App < keys[j].App
		}
		return keys[i].Hash < keys[j].Hash
	})
	return keys
}

func classifyOne(in Input, tiers []model.Tier, t qbit.Torrent) *Download {
	d := &Download{Torrent: t, Group: cleanPath(t.ContentPath)}

	tier, tierPath, ok := locate(t.SavePath, tiers)
	if !ok {
		d.State, d.Reason = Ignored, "not in a downloads tier"
		return d
	}
	d.Tier, d.TierPath = tier, tierPath

	labels := matchLabels(t, in.Config)
	if labels.ignoredBy != "" {
		d.State, d.Reason = Ignored, fmt.Sprintf("labeled %q, which Coldarr leaves alone", labels.ignoredBy)
		return d
	}
	if !labels.managed {
		d.State, d.Reason = Ignored, unmappedReason(t)
		return d
	}
	d.ImportedBy = labels.importedBy

	if reason, busy := activeReason(t, in.Queued); busy {
		d.State, d.Reason = Active, reason
		return d
	}
	d.Seeding = seedingStatus(t, in.Trackers, in.Config.Seeding, in.Now)
	if d.Seeding.Met {
		// Done seeding means a removal candidate, wherever it is - and
		// removing a download its importer hasn't imported loses it.
		d.importsReason(in)
	}

	if tier.Role == model.RoleCold {
		d.State = Parked
		if t.Category != in.Config.SeedLabel {
			if reason := d.clearToTouch(in); reason != "" {
				d.Reason = "not relabeled yet: " + reason
			} else {
				d.Relabel = true
			}
		}
		return d
	}

	d.SettledAt = settledAt(t, in.Config.SettleDays, in.Now)
	switch {
	case d.SettledAt.IsZero():
		d.State, d.Reason = Held, "qBittorrent doesn't say when it finished"
		return d
	case in.Now.Before(d.SettledAt):
		d.State = Waiting
		return d
	}

	if reason := d.clearToTouch(in); reason != "" {
		d.State, d.Reason = Held, reason
		return d
	}
	if exists, err := in.FS.Exists(t.ContentPath); err != nil {
		d.State, d.Reason = Held, fmt.Sprintf("checking %s: %v", t.ContentPath, err)
		return d
	} else if !exists {
		d.State, d.Reason = Held, fmt.Sprintf("Coldarr can't see it at %s - is %s mounted into Coldarr at the same path qBittorrent uses?", t.ContentPath, d.TierPath)
		return d
	}
	d.State = Ready
	return d
}

// clearToTouch runs the checks a download must pass before Coldarr moves
// or relabels it, returning why it can't - or "" if it can. As a side
// effect it records the import checks still to run, and whether
// Automatic Torrent Management has to be switched off first.
func (d *Download) clearToTouch(in Input) string {
	if d.AutoTMM {
		if !in.Config.AllowDisablingAutoTMM {
			return "Automatic Torrent Management is on, so qBittorrent would undo the move - turn it off for this download, or let Coldarr do that under Settings > Downloads"
		}
		d.DisableAutoTMM = true
	}
	return d.importsReason(in)
}

// importsReason says why the download's imports aren't confirmed, or ""
// if they are - recording any checks still to run, once.
func (d *Download) importsReason(in Input) string {
	if d.importChecked {
		return d.importReason
	}
	d.importChecked = true
	d.importReason = d.checkImports(in)
	d.ImportsConfirmed = d.importReason == ""
	return d.importReason
}

func (d *Download) checkImports(in Input) string {
	hash := strings.ToUpper(d.Hash)
	var waiting []string
	for _, app := range d.ImportedBy {
		if !in.Connected[app] {
			return fmt.Sprintf("%s isn't connected, so Coldarr can't confirm it imported this download", appName(app))
		}
		key := ImportKey{App: app, Hash: hash}
		res, checked := in.Imports[key]
		switch {
		case !checked:
			d.pendingImports = append(d.pendingImports, key)
			waiting = append(waiting, appName(app))
		case res.Err != "":
			return fmt.Sprintf("couldn't ask %s whether it imported this download: %s", appName(app), res.Err)
		case !res.Imported:
			return fmt.Sprintf("%s hasn't imported it yet", appName(app))
		}
	}
	if len(waiting) > 0 {
		return "not yet checked whether " + strings.Join(waiting, " and ") + " imported it"
	}
	return ""
}

type labelMatch struct {
	managed    bool
	ignoredBy  string
	importedBy []string
}

// matchLabels applies the label rules to a download's category and tags.
// An ignore rule wins over any move rule - "never touch" means never - and
// a download with no label that has a rule is left alone, so a category
// created next month is never acted on until someone says it should be.
// The seed label is always a move label: it's the one Coldarr gives
// downloads on seed storage.
func matchLabels(t qbit.Torrent, cfg config.TorrentsConfig) labelMatch {
	var m labelMatch
	for _, label := range t.Labels() {
		rule, ok := cfg.Rule(label)
		if !ok && label == cfg.SeedLabel {
			rule, ok = config.LabelRule{Label: label, Action: config.LabelMove}, true
		}
		if !ok {
			continue
		}
		switch rule.Action {
		case config.LabelIgnore:
			return labelMatch{ignoredBy: label}
		case config.LabelMove:
			m.managed = true
			if rule.ImportedBy != "" && !contains(m.importedBy, rule.ImportedBy) {
				m.importedBy = append(m.importedBy, rule.ImportedBy)
			}
		}
	}
	return m
}

func unmappedReason(t qbit.Torrent) string {
	labels := t.Labels()
	if len(labels) == 0 {
		return "no category or tag"
	}
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = fmt.Sprintf("%q", l)
	}
	return "no rule for " + strings.Join(quoted, ", ")
}

// activeReason reports whether the download is busy - anything that means
// it must not be touched right now - and why.
func activeReason(t qbit.Torrent, queued map[string]string) (string, bool) {
	switch {
	case t.State == "moving":
		return "qBittorrent is moving it", true
	case t.State == "allocating":
		return "qBittorrent is allocating its files", true
	case qbit.InFlight(t.State):
		return "qBittorrent is checking it", true
	case qbit.Errored(t.State):
		return fmt.Sprintf("qBittorrent reports %s", t.State), true
	case !t.Complete():
		return fmt.Sprintf("downloading, %.0f%% done", t.Progress*100), true
	case !qbit.Seeding(t.State):
		// A complete download in a state this version doesn't know:
		// leave it alone rather than guess what the state means.
		return fmt.Sprintf("in a state Coldarr doesn't recognize (%q)", t.State), true
	}
	if app, ok := queued[strings.ToUpper(t.Hash)]; ok {
		return fmt.Sprintf("still in %s's queue", appName(app)), true
	}
	return "", false
}

// settledAt is when the download will have been finished for settleDays.
// qBittorrent's completion time is the clock that counts; failing that,
// time spent seeding is a lower bound on time since finishing, so it errs
// toward waiting longer. Zero when neither is known.
func settledAt(t qbit.Torrent, settleDays int, now time.Time) time.Time {
	settle := time.Duration(settleDays) * 24 * time.Hour
	switch {
	case !t.CompletedOn.IsZero():
		return t.CompletedOn.Add(settle)
	case t.SeedingTimeKnown:
		return now.Add(-t.SeedingTime).Add(settle)
	}
	return time.Time{}
}

// holdGroups applies the cross-seed rule: downloads sharing files move
// together or not at all. A group moves only if every member is ready,
// and a download whose files sit inside another download's folder (or
// whose folder holds another's files) can't be moved safely as either a
// group or alone - one move would pull the files out from under the
// other.
func holdGroups(downloads []*Download) {
	groups := map[string][]*Download{}
	for _, d := range downloads {
		if d.TierPath == "" || d.Group == "" {
			continue
		}
		groups[d.Group] = append(groups[d.Group], d)
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Sorted, a path's descendants follow it directly (after any sibling
	// whose name merely starts with the same characters), so comparing
	// each key with those after it, until the prefix stops matching,
	// finds every nested pair.
	for i, outer := range keys {
		for _, inner := range keys[i+1:] {
			if !strings.HasPrefix(inner, outer) {
				break
			}
			if !isUnder(inner, outer) {
				continue
			}
			holdAll(groups[outer], "shares files with "+groups[inner][0].Name+", which sits inside its folder")
			holdAll(groups[inner], "shares files with "+groups[outer][0].Name+", whose folder holds them")
		}
	}

	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		var blocker *Download
		for _, m := range members {
			if m.State != Ready {
				blocker = m
				break
			}
		}
		if blocker == nil {
			continue
		}
		for _, m := range members {
			if m.State == Ready {
				m.State = Held
				m.Reason = fmt.Sprintf("cross-seeded with %s, which is %s", blocker.Name, describe(blocker))
			}
		}
	}
}

func holdAll(members []*Download, reason string) {
	for _, m := range members {
		if m.State == Ready {
			m.State, m.Reason = Held, reason
		}
	}
}

// describe says what state a cross-seed partner is in, for a held
// download's reason.
func describe(d *Download) string {
	switch d.State {
	case Ignored:
		return "left alone (" + d.Reason + ")"
	case Waiting:
		return "still settling"
	case Parked:
		return "already on seed storage"
	case Active, Held:
		return d.Reason
	}
	return string(d.State)
}

// locate finds the downloads tier a save path sits under, and which of
// its configured paths. The deepest configured path wins, so a tier path
// nested inside another tier's is still matched to its own tier.
func locate(savePath string, tiers []model.Tier) (model.Tier, string, bool) {
	clean := cleanPath(savePath)
	if clean == "" {
		return model.Tier{}, "", false
	}
	var best model.Tier
	bestPath := ""
	for _, tier := range tiers {
		for _, p := range tier.Paths {
			p = cleanPath(p)
			if isUnder(clean, p) && len(p) > len(bestPath) {
				best, bestPath = tier, p
			}
		}
	}
	return best, bestPath, bestPath != ""
}

func cleanPath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// isUnder reports whether path is root itself or a descendant of it.
func isUnder(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// appName is an *arr app's name as it reads in a sentence.
func appName(app string) string {
	switch app {
	case "radarr":
		return "Radarr"
	case "sonarr":
		return "Sonarr"
	case "lidarr":
		return "Lidarr"
	}
	return app
}
