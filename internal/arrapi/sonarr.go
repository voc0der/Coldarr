package arrapi

import (
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/model"
)

type SonarrClient struct {
	c *client
}

func NewSonarrClient(baseURL, apiKey string) *SonarrClient {
	return &SonarrClient{c: newClient(baseURL, apiKey)}
}

// Ping confirms the connection works and returns Sonarr's reported version.
func (s *SonarrClient) Ping() (version string, err error) {
	return s.c.ping()
}

type sonarrSeriesStatistics struct {
	SizeOnDisk       int64 `json:"sizeOnDisk"`
	EpisodeFileCount int   `json:"episodeFileCount"`
}

type sonarrSeries struct {
	ID               int                    `json:"id"`
	Title            string                 `json:"title"`
	TitleSlug        string                 `json:"titleSlug"`
	Path             string                 `json:"path"`
	RootFolderPath   string                 `json:"rootFolderPath"`
	QualityProfileID int                    `json:"qualityProfileId"`
	Monitored        bool                   `json:"monitored"`
	Added            time.Time              `json:"added"`
	Tags             []int                  `json:"tags"`
	Status           string                 `json:"status"`
	Ended            bool                   `json:"ended"`
	PreviousAiring   *time.Time             `json:"previousAiring"`
	Statistics       sonarrSeriesStatistics `json:"statistics"`
}

type sonarrQueueRecord struct {
	SeriesID int `json:"seriesId"`
}

type sonarrQueuePage struct {
	Records []sonarrQueueRecord `json:"records"`
}

type sonarrWantedCutoffRecord struct {
	SeriesID int `json:"seriesId"`
}

type sonarrWantedCutoffPage struct {
	Records      []sonarrWantedCutoffRecord `json:"records"`
	TotalRecords int                        `json:"totalRecords"`
}

// FetchSeries returns every series Sonarr knows about, normalized into
// MediaItems with tag labels resolved and active-queue state filled in.
// The five lookups it needs are independent, so they run concurrently -
// sequentially they add up to five round trips of network latency on
// every plan/dashboard page load.
func (s *SonarrClient) FetchSeries() ([]model.MediaItem, error) {
	var (
		series      []sonarrSeries
		tags        []tagResource
		profiles    []qualityProfileResource
		busy        map[int]bool
		cutoffUnmet map[int]bool

		seriesErr, tagsErr, profilesErr, busyErr, cutoffErr error
	)

	var wg sync.WaitGroup
	wg.Add(5)
	go func() { defer wg.Done(); seriesErr = s.c.get("/api/v3/series", nil, &series) }()
	go func() { defer wg.Done(); tagsErr = s.c.get("/api/v3/tag", nil, &tags) }()
	go func() { defer wg.Done(); profilesErr = s.c.get("/api/v3/qualityprofile", nil, &profiles) }()
	go func() { defer wg.Done(); busy, busyErr = s.BusySeriesIDs() }()
	go func() { defer wg.Done(); cutoffUnmet, cutoffErr = s.CutoffUnmetSeriesIDs() }()
	wg.Wait()

	if seriesErr != nil {
		return nil, seriesErr
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

	items := make([]model.MediaItem, 0, len(series))
	for _, sr := range series {
		ended := sr.Ended || sr.Status == "ended"
		upcoming := sr.Status == "upcoming"
		items = append(items, model.MediaItem{
			ArrApp:              "sonarr",
			ID:                  sr.ID,
			Type:                model.TV,
			Title:               sr.Title,
			TitleSlug:           sr.TitleSlug,
			Path:                sr.Path,
			RootFolderPath:      sr.RootFolderPath,
			SizeBytes:           sr.Statistics.SizeOnDisk,
			Added:               sr.Added,
			Tags:                tagLabels(sr.Tags, tagByID),
			QualityProfileName:  profileByID[sr.QualityProfileID],
			Monitored:           sr.Monitored,
			HasFile:             sr.Statistics.EpisodeFileCount > 0,
			Ended:               ended,
			Upcoming:            upcoming,
			LastAired:           sr.PreviousAiring,
			InActiveQueue:       busy[sr.ID],
			QualityCutoffNotMet: cutoffUnmet[sr.ID],
		})
	}
	return items, nil
}

// TitleSlugs returns every series' titleSlug keyed by Sonarr's internal
// ID - used by the History page (which only records the ID) to build a
// deep link into Sonarr's web UI without paying for FetchSeries' extra
// tag/quality-profile/queue round trips, which a link has no use for.
func (s *SonarrClient) TitleSlugs() (map[int]string, error) {
	var series []sonarrSeries
	if err := s.c.get("/api/v3/series", nil, &series); err != nil {
		return nil, err
	}
	slugs := make(map[int]string, len(series))
	for _, sr := range series {
		slugs[sr.ID] = sr.TitleSlug
	}
	return slugs, nil
}

// GetSeriesSize returns the size and current folder Sonarr reports for
// series id, straight from Sonarr's own database - used to verify a past
// move actually landed intact rather than being interrupted partway (e.g.
// by a crash). found is false if Sonarr no longer knows about this series
// (it was deleted or replaced since Coldarr moved it), which is not an
// error.
func (s *SonarrClient) GetSeriesSize(id int) (sizeBytes int64, path string, found bool, err error) {
	var sr sonarrSeries
	if err := s.c.get(fmt.Sprintf("/api/v3/series/%d", id), nil, &sr); err != nil {
		if IsNotFound(err) {
			return 0, "", false, nil
		}
		return 0, "", false, err
	}
	return sr.Statistics.SizeOnDisk, sr.Path, true, nil
}

// RescanSeries asks Sonarr to rescan series id's folder on disk,
// refreshing its cached file/size info from the real filesystem, and
// blocks until the rescan finishes - used before a "complete" size
// verification so Sonarr's own view is current before Coldarr also
// independently checks the folder itself.
func (s *SonarrClient) RescanSeries(id int) error {
	return s.c.runCommand(map[string]any{"name": "RescanSeries", "seriesId": id})
}

// BusySeriesIDs returns the set of series IDs Sonarr currently has an
// active download, import, or move in progress for.
func (s *SonarrClient) BusySeriesIDs() (map[int]bool, error) {
	q := url.Values{}
	q.Set("pageSize", "1000")
	q.Set("includeUnknownSeriesItems", "true")
	var queue sonarrQueuePage
	if err := s.c.get("/api/v3/queue", q, &queue); err != nil {
		return nil, err
	}
	busy := make(map[int]bool, len(queue.Records))
	for _, rec := range queue.Records {
		if rec.SeriesID != 0 {
			busy[rec.SeriesID] = true
		}
	}
	return busy, nil
}

// CutoffUnmetSeriesIDs returns the set of series IDs with at least one
// episode file that doesn't meet its quality profile's upgrade cutoff -
// Sonarr will keep searching for a better release for those episodes, so
// the series' folder (and size) isn't settled yet. Restricted to
// monitored episodes since that's the only case Sonarr actually searches.
// Paginates through the whole result set: unlike Radarr's movie-level
// cutoff, Sonarr's is per-episode, so a large library can easily have
// more cutoff-unmet episodes than fit on one page.
func (s *SonarrClient) CutoffUnmetSeriesIDs() (map[int]bool, error) {
	const pageSize = 250
	unmet := map[int]bool{}
	fetched := 0
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("pageSize", strconv.Itoa(pageSize))
		q.Set("monitored", "true")
		var got sonarrWantedCutoffPage
		if err := s.c.get("/api/v3/wanted/cutoff", q, &got); err != nil {
			return nil, err
		}
		for _, rec := range got.Records {
			if rec.SeriesID != 0 {
				unmet[rec.SeriesID] = true
			}
		}
		fetched += len(got.Records)
		if len(got.Records) == 0 || fetched >= got.TotalRecords {
			break
		}
	}
	return unmet, nil
}

type seriesEditorRequest struct {
	SeriesIDs      []int  `json:"seriesIds"`
	RootFolderPath string `json:"rootFolderPath"`
	MoveFiles      bool   `json:"moveFiles"`
}

// MoveSeries asks Sonarr to relocate the given series to rootFolderPath,
// moving files on disk and keeping Sonarr's database in sync.
func (s *SonarrClient) MoveSeries(seriesIDs []int, rootFolderPath string) error {
	if len(seriesIDs) == 0 {
		return nil
	}
	req := seriesEditorRequest{
		SeriesIDs:      seriesIDs,
		RootFolderPath: rootFolderPath,
		MoveFiles:      true,
	}
	return s.c.put("/api/v3/series/editor", req, nil)
}
