package arrapi

import (
	"net/url"
	"strconv"
	"strings"
)

// The calls in this file look at downloads from the *arr side: what an app
// is still importing, and what it has already imported. Each app records a
// download by its client's ID - for qBittorrent the torrent's hash,
// upper-cased - so these are how Coldarr joins a torrent to the app that
// grabbed it, without matching on names or paths.

type queueDownloadRecord struct {
	DownloadID string `json:"downloadId"`
}

type queueDownloadPage struct {
	Records      []queueDownloadRecord `json:"records"`
	TotalRecords int                   `json:"totalRecords"`
}

// queueDownloadIDs returns the download ID of every item in the app's
// queue, upper-cased - downloads it is still fetching or has yet to import.
// unknownParam names the app's switch for including downloads it can't
// match to a library item, which are queued all the same (and are exactly
// the ones waiting on a manual import). Paginates, so a long queue can't
// hide a download past the first page.
func (c *client) queueDownloadIDs(unknownParam string) (map[string]bool, error) {
	const pageSize = 250
	ids := map[string]bool{}
	fetched := 0
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("pageSize", strconv.Itoa(pageSize))
		q.Set(unknownParam, "true")
		var got queueDownloadPage
		if err := c.get(c.api+"/queue", q, &got); err != nil {
			return nil, err
		}
		for _, rec := range got.Records {
			if rec.DownloadID != "" {
				ids[strings.ToUpper(rec.DownloadID)] = true
			}
		}
		fetched += len(got.Records)
		if len(got.Records) == 0 || fetched >= got.TotalRecords {
			break
		}
	}
	return ids, nil
}

type historyRecord struct {
	DownloadID string `json:"downloadId"`
	EventType  string `json:"eventType"`
}

type historyPage struct {
	Records []historyRecord `json:"records"`
}

// imported reports whether the app's history holds one of importEvents for
// downloadID. The apps filter history by exact download ID, so the ID is
// sent upper-cased the way they store it - and each record's own ID is
// checked again here, so an app that ignored the filter would return its
// latest history without that being taken for this download's import.
func (c *client) imported(downloadID string, importEvents ...string) (bool, error) {
	id := strings.ToUpper(downloadID)
	q := url.Values{}
	q.Set("downloadId", id)
	q.Set("pageSize", "100")
	var got historyPage
	if err := c.get(c.api+"/history", q, &got); err != nil {
		return false, err
	}
	for _, rec := range got.Records {
		if !strings.EqualFold(rec.DownloadID, id) {
			continue
		}
		for _, event := range importEvents {
			if rec.EventType == event {
				return true, nil
			}
		}
	}
	return false, nil
}

// QueueDownloadIDs returns the upper-cased download ID of everything in
// Radarr's queue - see queueDownloadIDs.
func (r *RadarrClient) QueueDownloadIDs() (map[string]bool, error) {
	return r.c.queueDownloadIDs("includeUnknownMovieItems")
}

// Imported reports whether Radarr has imported the download - see imported.
func (r *RadarrClient) Imported(downloadID string) (bool, error) {
	return r.c.imported(downloadID, "downloadFolderImported")
}

// QueueDownloadIDs returns the upper-cased download ID of everything in
// Sonarr's queue - see queueDownloadIDs.
func (s *SonarrClient) QueueDownloadIDs() (map[string]bool, error) {
	return s.c.queueDownloadIDs("includeUnknownSeriesItems")
}

// Imported reports whether Sonarr has imported the download - see imported.
// Sonarr records one import per episode; any of them counts, since a
// download it hasn't finished importing stays in its queue.
func (s *SonarrClient) Imported(downloadID string) (bool, error) {
	return s.c.imported(downloadID, "downloadFolderImported")
}

// LidarrClient reads Lidarr's v1 API. Coldarr doesn't tier music libraries:
// Lidarr is only asked what it is still importing and what it already has,
// before Coldarr moves a music download.
type LidarrClient struct {
	c *client
}

func NewLidarrClient(baseURL, apiKey string) *LidarrClient {
	c := newClient(baseURL, apiKey)
	c.api = "/api/v1"
	return &LidarrClient{c: c}
}

// Ping confirms the connection works and returns Lidarr's reported version.
func (l *LidarrClient) Ping() (version string, err error) {
	return l.c.ping()
}

// QueueDownloadIDs returns the upper-cased download ID of everything in
// Lidarr's queue - see queueDownloadIDs.
func (l *LidarrClient) QueueDownloadIDs() (map[string]bool, error) {
	return l.c.queueDownloadIDs("includeUnknownArtistItems")
}

// Imported reports whether Lidarr has imported the download. Lidarr records
// each track as it lands and the download as a whole once every track has,
// so only the whole-download event counts - a download it only partly
// imported has not been.
func (l *LidarrClient) Imported(downloadID string) (bool, error) {
	return l.c.imported(downloadID, "downloadImported")
}

type lidarrArtist struct {
	Path string `json:"path"`
}

// ArtistPaths returns the folder of every artist Lidarr manages - content
// any configured service knows about, so never an orphan.
func (l *LidarrClient) ArtistPaths() ([]string, error) {
	var artists []lidarrArtist
	if err := l.c.get(l.c.api+"/artist", nil, &artists); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(artists))
	for _, a := range artists {
		if a.Path != "" {
			paths = append(paths, a.Path)
		}
	}
	return paths, nil
}
