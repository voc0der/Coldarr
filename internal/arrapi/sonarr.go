package arrapi

import (
	"net/url"
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
func (s *SonarrClient) FetchSeries() ([]model.MediaItem, error) {
	var series []sonarrSeries
	if err := s.c.get("/api/v3/series", nil, &series); err != nil {
		return nil, err
	}

	var tags []tagResource
	if err := s.c.get("/api/v3/tag", nil, &tags); err != nil {
		return nil, err
	}
	tagByID := make(map[int]string, len(tags))
	for _, t := range tags {
		tagByID[t.ID] = t.Label
	}

	var profiles []qualityProfileResource
	if err := s.c.get("/api/v3/qualityprofile", nil, &profiles); err != nil {
		return nil, err
	}
	profileByID := make(map[int]string, len(profiles))
	for _, p := range profiles {
		profileByID[p.ID] = p.Name
	}

	busy, err := s.BusySeriesIDs()
	if err != nil {
		return nil, err
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
