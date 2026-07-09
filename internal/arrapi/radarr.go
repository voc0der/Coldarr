package arrapi

import (
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/model"
)

type RadarrClient struct {
	c *client
}

func NewRadarrClient(baseURL, apiKey string) *RadarrClient {
	return &RadarrClient{c: newClient(baseURL, apiKey)}
}

// Ping confirms the connection works and returns Radarr's reported version.
func (r *RadarrClient) Ping() (version string, err error) {
	return r.c.ping()
}

type radarrMovie struct {
	ID               int       `json:"id"`
	Title            string    `json:"title"`
	TitleSlug        string    `json:"titleSlug"`
	Path             string    `json:"path"`
	RootFolderPath   string    `json:"rootFolderPath"`
	QualityProfileID int       `json:"qualityProfileId"`
	Monitored        bool      `json:"monitored"`
	HasFile          bool      `json:"hasFile"`
	Added            time.Time `json:"added"`
	Tags             []int     `json:"tags"`
	SizeOnDisk       int64     `json:"sizeOnDisk"`
	// Status is Radarr's MovieStatusType: "tba", "announced",
	// "inCinemas", or "released" - anything short of "released" means no
	// home-viewing file is expected yet.
	Status string `json:"status"`
}

// upcomingMovieStatuses are Radarr's MovieStatusType values that mean "not
// yet released for home viewing" - an allow-list rather than checking
// Status != "released", so an unrecognized/future status string fails
// safe (not treated as upcoming) instead of accidentally locking a movie
// onto hot storage forever.
var upcomingMovieStatuses = map[string]bool{
	"tba":       true,
	"announced": true,
	"inCinemas": true,
}

type radarrQueueRecord struct {
	MovieID int `json:"movieId"`
}

type radarrQueuePage struct {
	Records []radarrQueueRecord `json:"records"`
}

type radarrWantedCutoffRecord struct {
	ID int `json:"id"`
}

type radarrWantedCutoffPage struct {
	Records      []radarrWantedCutoffRecord `json:"records"`
	TotalRecords int                        `json:"totalRecords"`
}

// FetchMovies returns every movie Radarr knows about, normalized into
// MediaItems with tag labels resolved and active-queue state filled in.
// The five lookups it needs are independent, so they run concurrently -
// sequentially they add up to five round trips of network latency on
// every plan/dashboard page load.
func (r *RadarrClient) FetchMovies() ([]model.MediaItem, error) {
	var (
		movies      []radarrMovie
		tags        []tagResource
		profiles    []qualityProfileResource
		busy        map[int]bool
		cutoffUnmet map[int]bool

		moviesErr, tagsErr, profilesErr, busyErr, cutoffErr error
	)

	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); moviesErr = r.c.get("/api/v3/movie", nil, &movies) }()
	go func() { defer wg.Done(); tagsErr = r.c.get("/api/v3/tag", nil, &tags) }()
	go func() { defer wg.Done(); profilesErr = r.c.get("/api/v3/qualityprofile", nil, &profiles) }()
	go func() { defer wg.Done(); busy, busyErr = r.BusyMovieIDs() }()
	go func() { defer wg.Done(); cutoffUnmet, cutoffErr = r.CutoffUnmetMovieIDs() }()
	wg.Wait()

	if moviesErr != nil {
		return nil, moviesErr
	}
	if tagsErr != nil {
		return nil, tagsErr
	}
	if profilesErr != nil {
		return nil, profilesErr
	}
	if busyErr != nil {
		return nil, busyErr
	}
	if cutoffErr != nil {
		return nil, cutoffErr
	}

	tagByID := make(map[int]string, len(tags))
	for _, t := range tags {
		tagByID[t.ID] = t.Label
	}

	profileByID := make(map[int]string, len(profiles))
	for _, p := range profiles {
		profileByID[p.ID] = p.Name
	}

	items := make([]model.MediaItem, 0, len(movies))
	for _, m := range movies {
		items = append(items, model.MediaItem{
			ArrApp:              "radarr",
			ID:                  m.ID,
			Type:                model.Movie,
			Title:               m.Title,
			TitleSlug:           m.TitleSlug,
			Path:                m.Path,
			RootFolderPath:      m.RootFolderPath,
			SizeBytes:           m.SizeOnDisk,
			Added:               m.Added,
			Tags:                tagLabels(m.Tags, tagByID),
			QualityProfileName:  profileByID[m.QualityProfileID],
			Monitored:           m.Monitored,
			HasFile:             m.HasFile,
			Upcoming:            upcomingMovieStatuses[m.Status],
			InActiveQueue:       busy[m.ID],
			QualityCutoffNotMet: cutoffUnmet[m.ID],
		})
	}
	return items, nil
}

// TitleSlugs returns every movie's titleSlug keyed by Radarr's internal
// ID - used by the History page (which only records the ID) to build a
// deep link into Radarr's web UI without paying for FetchMovies' extra
// tag/quality-profile/queue round trips, which a link has no use for.
func (r *RadarrClient) TitleSlugs() (map[int]string, error) {
	var movies []radarrMovie
	if err := r.c.get("/api/v3/movie", nil, &movies); err != nil {
		return nil, err
	}
	slugs := make(map[int]string, len(movies))
	for _, m := range movies {
		slugs[m.ID] = m.TitleSlug
	}
	return slugs, nil
}

// GetMovieSize returns the size and current folder Radarr reports for
// movie id, straight from Radarr's own database - used to verify a past
// move actually landed intact rather than being interrupted partway (e.g.
// by a crash). found is false if Radarr no longer knows about this movie
// (it was deleted or replaced since Coldarr moved it), which is not an
// error.
func (r *RadarrClient) GetMovieSize(id int) (sizeBytes int64, path string, found bool, err error) {
	var m radarrMovie
	if err := r.c.get(fmt.Sprintf("/api/v3/movie/%d", id), nil, &m); err != nil {
		if IsNotFound(err) {
			return 0, "", false, nil
		}
		return 0, "", false, err
	}
	return m.SizeOnDisk, m.Path, true, nil
}

// RescanMovie asks Radarr to rescan movie id's folder on disk, refreshing
// its cached file/size info from the real filesystem, and blocks until
// the rescan finishes - used before a "complete" size verification so
// Radarr's own view is current before Coldarr also independently checks
// the folder itself.
func (r *RadarrClient) RescanMovie(id int) error {
	return r.c.runCommand(map[string]any{"name": "RescanMovie", "movieId": id})
}

// BusyMovieIDs returns the set of movie IDs Radarr currently has an active
// download, import, or move in progress for - used both to protect items
// from being planned for a move, and to confirm a move Coldarr just
// requested has actually finished before starting the next one.
func (r *RadarrClient) BusyMovieIDs() (map[int]bool, error) {
	q := url.Values{}
	q.Set("pageSize", "1000")
	q.Set("includeUnknownMovieItems", "true")
	var queue radarrQueuePage
	if err := r.c.get("/api/v3/queue", q, &queue); err != nil {
		return nil, err
	}
	busy := make(map[int]bool, len(queue.Records))
	for _, rec := range queue.Records {
		if rec.MovieID != 0 {
			busy[rec.MovieID] = true
		}
	}
	return busy, nil
}

// CutoffUnmetMovieIDs returns the set of movie IDs whose current file
// doesn't meet its quality profile's upgrade cutoff - Radarr will keep
// searching for a better release for these, so the file (and folder size)
// isn't settled yet. Restricted to monitored movies since that's the only
// case Radarr actually searches. Paginates through the whole result set:
// a large library after a profile change can plausibly have more
// cutoff-unmet movies than fit on one page.
func (r *RadarrClient) CutoffUnmetMovieIDs() (map[int]bool, error) {
	const pageSize = 250
	unmet := map[int]bool{}
	fetched := 0
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("pageSize", strconv.Itoa(pageSize))
		q.Set("monitored", "true")
		var got radarrWantedCutoffPage
		if err := r.c.get("/api/v3/wanted/cutoff", q, &got); err != nil {
			return nil, err
		}
		for _, rec := range got.Records {
			unmet[rec.ID] = true
		}
		fetched += len(got.Records)
		if len(got.Records) == 0 || fetched >= got.TotalRecords {
			break
		}
	}
	return unmet, nil
}

type movieEditorRequest struct {
	MovieIDs       []int  `json:"movieIds"`
	RootFolderPath string `json:"rootFolderPath"`
	MoveFiles      bool   `json:"moveFiles"`
}

// MoveMovies asks Radarr to relocate the given movies to rootFolderPath,
// moving files on disk and keeping Radarr's database in sync. Radarr keeps
// each movie's existing folder name, so this maps a set of movies from one
// tier's root folder onto another's.
func (r *RadarrClient) MoveMovies(movieIDs []int, rootFolderPath string) error {
	if len(movieIDs) == 0 {
		return nil
	}
	req := movieEditorRequest{
		MovieIDs:       movieIDs,
		RootFolderPath: rootFolderPath,
		MoveFiles:      true,
	}
	return r.c.put("/api/v3/movie/editor", req, nil)
}
