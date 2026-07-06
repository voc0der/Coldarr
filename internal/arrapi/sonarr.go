package arrapi

import (
	"net/url"
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

// FetchSeries returns every series Sonarr knows about, normalized into
// MediaItems with tag labels resolved and active-queue state filled in.
// The four lookups it needs are independent, so they run concurrently -
// sequentially they add up to four round trips of network latency on
// every plan/dashboard page load.
func (s *SonarrClient) FetchSeries() ([]model.MediaItem, error) {
	var (
		series   []sonarrSeries
		tags     []tagResource
		profiles []qualityProfileResource
		busy     map[int]bool

		seriesErr, tagsErr, profilesErr, busyErr error
	)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); seriesErr = s.c.get("/api/v3/series", nil, &series) }()
	go func() { defer wg.Done(); tagsErr = s.c.get("/api/v3/tag", nil, &tags) }()
	go func() { defer wg.Done(); profilesErr = s.c.get("/api/v3/qualityprofile", nil, &profiles) }()
	go func() { defer wg.Done(); busy, busyErr = s.BusySeriesIDs() }()
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
		items = append(items, model.MediaItem{
			ArrApp:             "sonarr",
			ID:                 sr.ID,
			Type:               model.TV,
			Title:              sr.Title,
			Path:               sr.Path,
			RootFolderPath:     sr.RootFolderPath,
			SizeBytes:          sr.Statistics.SizeOnDisk,
			Added:              sr.Added,
			Tags:               tagLabels(sr.Tags, tagByID),
			QualityProfileName: profileByID[sr.QualityProfileID],
			Monitored:          sr.Monitored,
			HasFile:            sr.Statistics.EpisodeFileCount > 0,
			Ended:              ended,
			LastAired:          sr.PreviousAiring,
			InActiveQueue:      busy[sr.ID],
		})
	}
	return items, nil
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
