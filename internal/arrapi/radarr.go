package arrapi

import (
	"net/url"
	"time"

	"github.com/vocoder/coldarr/internal/model"
)

type RadarrClient struct {
	c *client
}

func NewRadarrClient(baseURL, apiKey string) *RadarrClient {
	return &RadarrClient{c: newClient(baseURL, apiKey)}
}

type radarrMovie struct {
	ID               int       `json:"id"`
	Title            string    `json:"title"`
	Path             string    `json:"path"`
	RootFolderPath   string    `json:"rootFolderPath"`
	QualityProfileID int       `json:"qualityProfileId"`
	Monitored        bool      `json:"monitored"`
	HasFile          bool      `json:"hasFile"`
	Added            time.Time `json:"added"`
	Tags             []int     `json:"tags"`
	SizeOnDisk       int64     `json:"sizeOnDisk"`
}

type radarrQueueRecord struct {
	MovieID int `json:"movieId"`
}

type radarrQueuePage struct {
	Records []radarrQueueRecord `json:"records"`
}

// FetchMovies returns every movie Radarr knows about, normalized into
// MediaItems with tag labels resolved and active-queue state filled in.
func (r *RadarrClient) FetchMovies() ([]model.MediaItem, error) {
	var movies []radarrMovie
	if err := r.c.get("/api/v3/movie", nil, &movies); err != nil {
		return nil, err
	}

	var tags []tagResource
	if err := r.c.get("/api/v3/tag", nil, &tags); err != nil {
		return nil, err
	}
	tagByID := make(map[int]string, len(tags))
	for _, t := range tags {
		tagByID[t.ID] = t.Label
	}

	var profiles []qualityProfileResource
	if err := r.c.get("/api/v3/qualityprofile", nil, &profiles); err != nil {
		return nil, err
	}
	profileByID := make(map[int]string, len(profiles))
	for _, p := range profiles {
		profileByID[p.ID] = p.Name
	}

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

	items := make([]model.MediaItem, 0, len(movies))
	for _, m := range movies {
		items = append(items, model.MediaItem{
			ArrApp:             "radarr",
			ID:                 m.ID,
			Type:               model.Movie,
			Title:              m.Title,
			Path:               m.Path,
			RootFolderPath:     m.RootFolderPath,
			SizeBytes:          m.SizeOnDisk,
			Added:              m.Added,
			Tags:               tagLabels(m.Tags, tagByID),
			QualityProfileName: profileByID[m.QualityProfileID],
			Monitored:          m.Monitored,
			HasFile:            m.HasFile,
			InActiveQueue:      busy[m.ID],
		})
	}
	return items, nil
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
