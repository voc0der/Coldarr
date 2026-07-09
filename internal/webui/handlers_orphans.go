package webui

import (
	"net/http"
	"sort"
)

// orphanCandidateView is one row on the Orphaned Storage page.
type orphanCandidateView struct {
	Path      string
	TierName  string
	TierRole  string
	SizeBytes int64
}

// tierWritabilityView reports whether a tier path was writable as of the
// last scan, and why not if it wasn't - shown so an operator can see
// up front which paths deletion won't be available on, once milestone 2
// adds it, rather than finding out by trying.
type tierWritabilityView struct {
	Path     string
	Writable bool
	Error    string
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
}

func (s *Server) orphansPageData() orphansData {
	snap := s.orphanStore.Get()

	data := orphansData{Title: "Orphaned Storage", Warnings: snap.Warnings}
	if !snap.ScannedAt.IsZero() {
		data.ScannedAt = snap.ScannedAt.Format("2006-01-02 15:04")
	}

	for _, c := range snap.Candidates {
		data.Candidates = append(data.Candidates, orphanCandidateView{
			Path:      c.Path,
			TierName:  c.TierName,
			TierRole:  string(c.TierRole),
			SizeBytes: c.SizeBytes,
		})
		data.TotalBytes += c.SizeBytes
	}
	sort.Slice(data.Candidates, func(i, j int) bool { return data.Candidates[i].SizeBytes > data.Candidates[j].SizeBytes })

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
