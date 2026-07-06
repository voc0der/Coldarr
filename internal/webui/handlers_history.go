package webui

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/vocoder/coldarr/internal/history"
)

// historyPageSize bounds how many records a single History page (and so a
// single "Verify sizes" click) covers - the verify button only ever checks
// the page currently on screen, so cost stays flat regardless of how much
// history has piled up.
const historyPageSize = 25

type historyRowView struct {
	MovedAt  string
	Title    string
	Links    []linkView
	ArrApp   string
	FromTier string
	FromPath string
	ToTier   string
	ToPath   string
	Size     string
}

type historyData struct {
	Title      string
	Error      string
	Rows       []historyRowView
	Page       int
	TotalPages int
	TotalCount int
	HasPrev    bool
	PrevPage   int
	HasNext    bool
	NextPage   int
}

// paginate slices all into the given 1-indexed page of historyPageSize
// records, clamping page into range. all must already be sorted the way
// callers want page 1 to read (History.All() returns newest first).
func paginate(all []history.Record, page int) (pageRecords []history.Record, resolvedPage, totalPages int) {
	totalPages = max(1, (len(all)+historyPageSize-1)/historyPageSize)
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := min((page-1)*historyPageSize, len(all))
	end := min(start+historyPageSize, len(all))
	return all[start:end], page, totalPages
}

func parsePage(raw string) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func (s *Server) buildHistoryData(page int) historyData {
	data := historyData{Title: "History", Page: page}

	eng, err := s.newEngine()
	if err != nil {
		data.Error = err.Error()
		return data
	}

	all := eng.History.All()
	data.TotalCount = len(all)

	pageRecords, resolvedPage, totalPages := paginate(all, page)
	data.Page = resolvedPage
	data.TotalPages = totalPages
	data.HasPrev = resolvedPage > 1
	data.PrevPage = resolvedPage - 1
	data.HasNext = resolvedPage < totalPages
	data.NextPage = resolvedPage + 1

	linkSrc := s.buildLinkSources()
	linkSnap := s.linkCache.Get()
	radarrSlugByID := linkSnap.RadarrTitleSlugByID
	sonarrSlugByID := linkSnap.SonarrTitleSlugByID

	for _, rec := range pageRecords {
		var slug string
		switch rec.ArrApp {
		case "radarr":
			slug = radarrSlugByID[rec.ItemID]
		case "sonarr":
			slug = sonarrSlugByID[rec.ItemID]
		}

		data.Rows = append(data.Rows, historyRowView{
			MovedAt:  rec.MovedAt.Format("2006-01-02 15:04"),
			Title:    rec.Title,
			Links:    itemLinks(linkSrc, rec.ArrApp, slug, rec.ToPath),
			ArrApp:   rec.ArrApp,
			FromTier: rec.FromTier,
			FromPath: rec.FromPath,
			ToTier:   rec.ToTier,
			ToPath:   rec.ToPath,
			Size:     fmt.Sprintf("%.1f GB", float64(rec.SizeBytes)/(1<<30)),
		})
	}

	return data
}

func (s *Server) handleHistoryPage(w http.ResponseWriter, r *http.Request) {
	page := parsePage(r.URL.Query().Get("page"))
	s.render(w, "history", s.buildHistoryData(page))
}

// handleVerifyStart re-checks every record on one History page against
// Radarr/Sonarr's current on-disk size, in the background - see
// startVerify. It never blocks on the checks themselves.
func (s *Server) handleVerifyStart(w http.ResponseWriter, r *http.Request) {
	s.verifyMu.Lock()
	if s.currentVerify != nil && !s.currentVerify.Snapshot().Done {
		s.verifyMu.Unlock()
		http.Redirect(w, r, "/history/verify/status", http.StatusSeeOther)
		return
	}
	s.verifyMu.Unlock()

	page := parsePage(r.FormValue("page"))
	mode := parseVerifyMode(r.FormValue("mode"))

	eng, err := s.newEngine()
	if err != nil {
		s.render(w, "history", historyData{Title: "History", Error: err.Error()})
		return
	}

	pageRecords, _, _ := paginate(eng.History.All(), page)

	progress := startVerify(eng.Radarr, eng.Sonarr, pageRecords, mode)

	s.verifyMu.Lock()
	s.currentVerify = progress
	s.verifyMu.Unlock()

	http.Redirect(w, r, "/history/verify/status", http.StatusSeeOther)
}

type verifyStatusEntryView struct {
	Title      string
	ArrApp     string
	MovedAt    string
	RecordedGB string
	CurrentGB  string
	ArrGB      string
	Status     string
	SizeStatus string
	Note       string
	Err        string
}

type verifyStatusData struct {
	Title         string
	Mode          string
	NoRun         bool
	Running       bool
	Entries       []verifyStatusEntryView
	MatchCount    int
	MismatchCount int
}

func (s *Server) currentVerifyStatus() verifyStatusData {
	s.verifyMu.Lock()
	progress := s.currentVerify
	s.verifyMu.Unlock()

	if progress == nil {
		return verifyStatusData{Title: "Verify sizes", NoRun: true}
	}

	snap := progress.Snapshot()
	data := verifyStatusData{Title: "Verify sizes", Mode: string(snap.Mode), Running: !snap.Done}

	for _, e := range snap.Entries {
		view := verifyStatusEntryView{
			Title:      e.Record.Title,
			ArrApp:     e.Record.ArrApp,
			MovedAt:    e.Record.MovedAt.Format("2006-01-02 15:04"),
			RecordedGB: fmt.Sprintf("%.1f GB", float64(e.Record.SizeBytes)/(1<<30)),
			Status:     e.Status,
			SizeStatus: e.SizeStatus,
			Note:       e.Note,
			Err:        e.Err,
		}

		if e.HasArrSize {
			view.ArrGB = fmt.Sprintf("%.1f GB", float64(e.ArrSize)/(1<<30))
		}

		switch e.Status {
		case "done":
			switch e.SizeStatus {
			case "match":
				view.CurrentGB = fmt.Sprintf("%.1f GB", float64(e.CurrentSize)/(1<<30))
				data.MatchCount++
			case "grew":
				view.CurrentGB = fmt.Sprintf("%.1f GB", float64(e.CurrentSize)/(1<<30))
				view.Note = joinNotes(view.Note, "larger than when moved - codec upgrades are risky in cold storage, worth a look")
				data.MismatchCount++
			case "shrank":
				view.CurrentGB = fmt.Sprintf("%.1f GB", float64(e.CurrentSize)/(1<<30))
				view.Note = joinNotes(view.Note, "smaller than when moved - often just a re-compress or trimmed season, but also how a crash-interrupted transfer looks")
				data.MismatchCount++
			case "unknown":
				view.Note = joinNotes(view.Note, "no longer found in "+e.Record.ArrApp+" - may have been deleted or replaced since")
			}
		}

		data.Entries = append(data.Entries, view)
	}

	return data
}

func joinNotes(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + " - " + addition
}

func (s *Server) handleVerifyStatus(w http.ResponseWriter, r *http.Request) {
	s.render(w, "verify_status", s.currentVerifyStatus())
}

func (s *Server) handleVerifyStatusPartial(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "verify_status", "verify_status_table", s.currentVerifyStatus())
}
