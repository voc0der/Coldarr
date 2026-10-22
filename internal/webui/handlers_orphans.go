package webui

import (
	"fmt"
	"log"
	"net/http"
	"sort"

	"github.com/vocoder/coldarr/internal/model"
)

// orphanCandidateView is one row on the Orphaned Storage page.
type orphanCandidateView struct {
	Path      string
	TierName  string
	TierRole  string
	SizeBytes int64
	File      bool
	// Deletable offers a Delete button: the entry is in a download folder
	// whose tier is marked as not read-only. Whether Coldarr can actually
	// write there is shown separately, and DeleteDownload re-checks
	// everything anyway.
	Deletable bool
}

// tierWritabilityView reports whether a tier path was writable as of the
// last scan, and why not if it wasn't - shown so an operator can see up
// front which paths a delete would fail on, rather than finding out by
// trying.
type tierWritabilityView struct {
	Path     string
	Writable bool
	Error    string
}

// readOnlyDownloadTier is a downloads tier still marked read-only, listed
// so it's clear why its orphans have no Delete button.
type readOnlyDownloadTier struct {
	Name string
}

type orphansData struct {
	Title      string
	Saved      string
	Error      string
	ScannedAt  string
	Candidates []orphanCandidateView
	TotalBytes int64
	Tiers      []tierWritabilityView
	Warnings   []string

	// ShowDownloads shows the download-folders panel - only when
	// qBittorrent is connected, the one service that can account for
	// what's in them.
	ShowDownloads      bool
	Downloads          []orphanCandidateView
	DownloadsBytes     int64
	ReadOnlyDownloads  []readOnlyDownloadTier
	AnyDeletableFolder bool
}

func (s *Server) orphansPageData() orphansData {
	snap := s.orphanStore.Get()
	cfg := s.currentConfig()

	data := orphansData{Title: "Orphaned Storage", Warnings: snap.Warnings, ShowDownloads: s.downloadsEnabled()}
	if !snap.ScannedAt.IsZero() {
		data.ScannedAt = snap.ScannedAt.Format("2006-01-02 15:04")
	}

	allowDeletes := map[string]bool{}
	for _, t := range model.TiersIn(cfg.Tiers, model.DomainDownloads) {
		allowDeletes[t.Name] = t.AllowDeletes
		if t.AllowDeletes {
			data.AnyDeletableFolder = true
		} else {
			data.ReadOnlyDownloads = append(data.ReadOnlyDownloads, readOnlyDownloadTier{Name: t.Name})
		}
	}

	for _, c := range snap.Candidates {
		view := orphanCandidateView{
			Path:      c.Path,
			TierName:  c.TierName,
			TierRole:  string(c.TierRole),
			SizeBytes: c.SizeBytes,
			File:      c.File,
		}
		if c.IsDownloads() {
			view.Deletable = allowDeletes[c.TierName]
			data.Downloads = append(data.Downloads, view)
			data.DownloadsBytes += c.SizeBytes
			continue
		}
		data.Candidates = append(data.Candidates, view)
		data.TotalBytes += c.SizeBytes
	}
	bySize := func(rows []orphanCandidateView) {
		sort.Slice(rows, func(i, j int) bool { return rows[i].SizeBytes > rows[j].SizeBytes })
	}
	bySize(data.Candidates)
	bySize(data.Downloads)

	paths := make([]string, 0, len(snap.TierWritable))
	for path := range snap.TierWritable {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		data.Tiers = append(data.Tiers, tierWritabilityView{
			Path:     path,
			Writable: snap.TierWritable[path],
			Error:    snap.TierWriteError[path],
		})
	}

	return data
}

func (s *Server) handleOrphansPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "settings_orphans", s.orphansPageData())
}

// handleOrphansScanNow is the Orphaned Storage page's own convenience
// "Scan now" button - the same scan as the Scheduler settings page's
// button (see scanOrphansNow), just rendering back to this page instead
// of Settings > Scheduler, so triggering a fresh look doesn't bounce an
// operator elsewhere.
func (s *Server) handleOrphansScanNow(w http.ResponseWriter, r *http.Request) {
	if err := s.scanOrphansNow(); err != nil {
		data := s.orphansPageData()
		data.Error = err.Error()
		s.render(w, "settings_orphans", data)
		return
	}

	data := s.orphansPageData()
	data.Saved = "Scan finished."
	s.render(w, "settings_orphans", data)
}

// handleOrphanDelete deletes one entry the last scan found in a download
// folder - see orphans.Store.DeleteDownload for every check it makes
// first. It holds scanOrphansMu so a delete and a scan never run at once:
// the scan would publish a list that still has the deleted entry, or the
// delete would act on a list being replaced under it.
func (s *Server) handleOrphanDelete(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	path := r.FormValue("path")

	render := func(saved string, err error) {
		data := s.orphansPageData()
		if err != nil {
			data.Error = err.Error()
		} else {
			data.Saved = saved
		}
		s.render(w, "settings_orphans", data)
	}

	if !s.scanOrphansMu.TryLock() {
		render("", fmt.Errorf("a scan is in progress - try again once it's finished"))
		return
	}
	defer s.scanOrphansMu.Unlock()

	eng, err := s.newEngine()
	if err != nil {
		render("", err)
		return
	}
	deleted, err := s.orphanStore.DeleteDownload(path, orphanSources(eng), eng.Cfg.Tiers)
	if err != nil {
		log.Printf("orphans: refused to delete %q: %q", path, err)
		render("", err)
		return
	}
	log.Printf("orphans: deleted %q (%.1f GB) from tier %q, as asked on the Orphaned Storage page", deleted.Path, float64(deleted.SizeBytes)/(1<<30), deleted.TierName)
	render(fmt.Sprintf("Deleted %s.", deleted.Path), nil)
}
