// Package jellyfin re-points Jellyfin at media Coldarr has moved between
// tiers (see NotifyMoved), reads Favorite status (matched back to
// Radarr/Sonarr items by path) so favorited items are never moved, and
// confirms connectivity. Jellyfin is a consumer of the library, never the
// mover.
package jellyfin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client

	// Logf records every write request this client makes - endpoint,
	// resolved item ID, query params, response status. A post-move refresh
	// that silently targets the wrong item (or no item) is otherwise
	// invisible: Jellyfin answers 204 for a refresh it will do nothing
	// with, so the status code alone proves nothing. Replaceable in tests.
	Logf func(format string, args ...any)

	// ResolvePollInterval and ResolveTimeout bound how long NotifyMoved
	// waits for Jellyfin to notice a moved item at its new path. Jellyfin
	// debounces filesystem-change reports before acting on them, so the
	// new item is never visible immediately after the move lands.
	ResolvePollInterval time.Duration
	ResolveTimeout      time.Duration
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:             strings.TrimRight(baseURL, "/"),
		apiKey:              apiKey,
		http:                &http.Client{Timeout: 30 * time.Second},
		Logf:                log.Printf,
		ResolvePollInterval: 10 * time.Second,
		ResolveTimeout:      3 * time.Minute,
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) get(path string, query url.Values) ([]byte, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("X-Emby-Token", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading response: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: unexpected status %d", path, resp.StatusCode)
	}
	return body, nil
}

// ErrItemNotFound is returned by RefreshItem when Jellyfin answers 404 -
// the item ID no longer exists. Jellyfin derives an item's ID by hashing
// its path, so an ID captured before a move is guaranteed dead after it;
// callers treat this as "re-resolve by the new path", never as a hard
// failure.
var ErrItemNotFound = errors.New("jellyfin: item not found")

// post issues a POST, logging the request and its outcome - endpoint,
// query params, status, and any response body. These endpoints answer 204
// with no content on success, so the body is only ever worth seeing when
// something went wrong; it's logged rather than returned. body may be nil
// for endpoints that take none.
func (c *Client) post(path string, query url.Values, body []byte) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, u, reader)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("X-Emby-Token", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		c.logf("jellyfin: POST %s?%s failed: %v", path, query.Encode(), err)
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("POST %s: reading response: %w", path, err)
	}

	c.logf("jellyfin: POST %s?%s -> %d %s", path, query.Encode(), resp.StatusCode, strings.TrimSpace(string(respBody)))

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("POST %s: %w", path, ErrItemNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: unexpected status %d", path, resp.StatusCode)
	}
	return nil
}

// Refresh modes accepted by /Items/{itemId}/Refresh. Jellyfin's OpenAPI
// spec types both metadataRefreshMode and imageRefreshMode as the same
// MetadataRefreshMode enum - there is no separate ImageRefreshMode schema -
// so one set of constants covers both.
const (
	RefreshModeNone           = "None"
	RefreshModeValidationOnly = "ValidationOnly"
	RefreshModeDefault        = "Default"
	RefreshModeFullRefresh    = "FullRefresh"
)

// RefreshOptions is one /Items/{itemId}/Refresh request. The zero value is
// deliberately useless: Jellyfin defaults both modes to "None" when the
// params are omitted, making a bare refresh call a no-op that still
// answers 204. Build these with FullRefreshOptions rather than by hand.
type RefreshOptions struct {
	MetadataMode        string
	ImageMode           string
	ReplaceAllMetadata  bool
	ReplaceAllImages    bool
	RegenerateTrickplay bool
}

// FullRefreshOptions is the API equivalent of the web UI's "Refresh
// metadata" with "Replace existing images" ticked - the only combination
// that fixes artwork after a move.
//
// Replacing is what matters, not merely refreshing. A library scan
// (POST /Library/Refresh) runs in Jellyfin's "Default" mode with
// ReplaceAllImages false, which only fills in images it considers
// *missing*; it can never displace an image record that already exists but
// points into the tier the item just left. FullRefresh + replace discards
// the stale record and re-fetches.
//
// RegenerateTrickplay stays off: it re-transcodes the video to rebuild
// scrubbing thumbnails, which is far more expensive than a metadata fetch
// and unrelated to the missing-poster problem.
func FullRefreshOptions() RefreshOptions {
	return RefreshOptions{
		MetadataMode:       RefreshModeFullRefresh,
		ImageMode:          RefreshModeFullRefresh,
		ReplaceAllMetadata: true,
		ReplaceAllImages:   true,
	}
}

func (o RefreshOptions) query() url.Values {
	q := url.Values{}
	q.Set("metadataRefreshMode", o.MetadataMode)
	q.Set("imageRefreshMode", o.ImageMode)
	q.Set("replaceAllMetadata", strconv.FormatBool(o.ReplaceAllMetadata))
	q.Set("replaceAllImages", strconv.FormatBool(o.ReplaceAllImages))
	q.Set("regenerateTrickplay", strconv.FormatBool(o.RegenerateTrickplay))
	return q
}

// RefreshItem re-fetches metadata and images for a single item. Returns
// ErrItemNotFound if Jellyfin no longer knows the ID.
func (c *Client) RefreshItem(itemID string, opts RefreshOptions) error {
	if itemID == "" {
		return fmt.Errorf("refreshing item: empty item ID")
	}
	return c.post("/Items/"+url.PathEscape(itemID)+"/Refresh", opts.query(), nil)
}

// RefreshLibrary triggers a full library scan. This is the fallback for
// when an item can't be resolved by path, not the primary notification:
// the scan walks every library root and runs in Jellyfin's "Default"
// refresh mode, so it cannot replace stale artwork (see
// FullRefreshOptions), and the 204 it returns means only "a scan started".
func (c *Client) RefreshLibrary() error {
	return c.post("/Library/Refresh", nil, nil)
}

// mediaUpdate is one entry of MediaUpdateInfoDto.
type mediaUpdate struct {
	Path       string `json:"Path"`
	UpdateType string `json:"UpdateType"`
}

// ReportMediaUpdated tells Jellyfin that specific paths changed on disk,
// so it rescans just those folders instead of the whole library.
//
// Jellyfin's handler for this endpoint passes every entry's Path to
// ReportFileSystemChanged and ignores UpdateType entirely - the DTO
// documents "Created, Modified, Deleted", but no server code reads the
// field. Only the paths do any work; UpdateType is populated for
// correctness against the schema, not because it changes behaviour.
func (c *Client) ReportMediaUpdated(paths []string, updateType string) error {
	updates := make([]mediaUpdate, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		updates = append(updates, mediaUpdate{Path: p, UpdateType: updateType})
	}
	if len(updates) == 0 {
		return nil
	}

	body, err := json.Marshal(struct {
		Updates []mediaUpdate `json:"Updates"`
	}{Updates: updates})
	if err != nil {
		return fmt.Errorf("encoding media update: %w", err)
	}

	return c.post("/Library/Media/Updated", nil, body)
}

// Ping confirms the connection works and returns Jellyfin's reported
// version and server name.
func (c *Client) Ping() (version, serverName string, err error) {
	body, err := c.get("/System/Info", nil)
	if err != nil {
		return "", "", err
	}

	var info struct {
		Version    string `json:"Version"`
		ServerName string `json:"ServerName"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", "", fmt.Errorf("GET /System/Info: decoding response: %w", err)
	}
	return info.Version, info.ServerName, nil
}

type userResource struct {
	ID string `json:"Id"`
}

type libraryItem struct {
	ID   string `json:"Id"`
	Path string `json:"Path"`
	// Type is Jellyfin's BaseItemKind for this item - "Movie" or "Series"
	// here, since those are the only kinds requested. Needed because the
	// two kinds report Path differently (see itemFolderPath).
	Type string `json:"Type"`
}

// itemFolderPath returns the folder Radarr/Sonarr would know this item by.
// Jellyfin reports Path differently per item kind: a Series' Path is
// already its containing folder (a series has no single file), but a
// Movie's Path is the video file itself, one level inside the folder
// Radarr manages - matching on it directly against Radarr's (folder) Path
// never succeeds, silently breaking favorite protection and Jellyfin deep
// links for every movie.
func itemFolderPath(item libraryItem) string {
	if item.Type == "Movie" {
		return filepath.Clean(filepath.Dir(item.Path))
	}
	return filepath.Clean(item.Path)
}

type itemsResponse struct {
	Items []libraryItem `json:"Items"`
}

// perUserItems fans out "/Users/{id}/Items" with the given query across
// every Jellyfin user and returns the union of items seen by any of them,
// deduplicated by ID - a library-restricted user might not see every item,
// so this counts anything visible to at least one household member.
// Per-user lookups are independent, so they run concurrently rather than
// adding one round trip of network latency per user to every plan/
// dashboard page load.
func (c *Client) perUserItems(q url.Values) ([]libraryItem, error) {
	body, err := c.get("/Users", nil)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}

	var users []userResource
	if err := json.Unmarshal(body, &users); err != nil {
		return nil, fmt.Errorf("listing users: decoding response: %w", err)
	}

	perUser := make([][]libraryItem, len(users))
	errs := make([]error, len(users))

	var wg sync.WaitGroup
	wg.Add(len(users))
	for i, u := range users {
		go func(i int, userID string) {
			defer wg.Done()

			body, err := c.get("/Users/"+userID+"/Items", q)
			if err != nil {
				errs[i] = fmt.Errorf("listing items for user %s: %w", userID, err)
				return
			}

			var resp itemsResponse
			if err := json.Unmarshal(body, &resp); err != nil {
				errs[i] = fmt.Errorf("listing items for user %s: decoding response: %w", userID, err)
				return
			}
			perUser[i] = resp.Items
		}(i, u.ID)
	}
	wg.Wait()

	seen := map[string]bool{}
	var items []libraryItem
	for i, err := range errs {
		if err != nil {
			return nil, err
		}
		for _, item := range perUser[i] {
			if item.ID == "" || seen[item.ID] {
				continue
			}
			seen[item.ID] = true
			items = append(items, item)
		}
	}
	return items, nil
}

// FavoritePaths returns the folder path (see itemFolderPath) of every
// movie and series marked as a Favorite by any Jellyfin user, for matching
// back to Radarr/Sonarr items by path. Favorite status is per-user in
// Jellyfin's API (there's no library-wide "is this a favorite" flag), so
// this enumerates every user and unions their favorites - if anyone in the
// household favorited it, Coldarr treats it as protected.
func (c *Client) FavoritePaths() (map[string]bool, error) {
	q := url.Values{}
	q.Set("Recursive", "true")
	q.Set("Filters", "IsFavorite")
	q.Set("IncludeItemTypes", "Movie,Series")
	q.Set("Fields", "Path")

	items, err := c.perUserItems(q)
	if err != nil {
		return nil, err
	}

	paths := map[string]bool{}
	for _, item := range items {
		if item.Path == "" {
			continue
		}
		paths[itemFolderPath(item)] = true
	}
	return paths, nil
}

// LibraryItemIDs returns every movie/series Jellyfin knows about, keyed by
// folder path (see itemFolderPath), mapped to Jellyfin's own internal item
// ID - used to build a deep link from a Radarr/Sonarr item into its
// Jellyfin entry, when the two can be matched by path.
func (c *Client) LibraryItemIDs() (map[string]string, error) {
	q := url.Values{}
	q.Set("Recursive", "true")
	q.Set("IncludeItemTypes", "Movie,Series")
	q.Set("Fields", "Path")

	items, err := c.perUserItems(q)
	if err != nil {
		return nil, err
	}

	ids := map[string]string{}
	for _, item := range items {
		if item.Path == "" || item.ID == "" {
			continue
		}
		ids[itemFolderPath(item)] = item.ID
	}
	return ids, nil
}

// MovedItem is one item's relocation, in the folder paths Jellyfin
// matches on (see itemFolderPath) - NOT the tier root the move targeted.
// A plan entry's ToPath is the destination tier root ("/mnt/sat1/movies"),
// which every item in that tier shares; refreshing it would target the
// library folder rather than the item.
type MovedItem struct {
	Title   string
	OldPath string
	NewPath string
}

// NotifyMoved brings Jellyfin back in sync after Coldarr relocates items
// between tiers, and is the reason a moved item keeps its artwork.
//
// The sequence matters, and none of it can be collapsed into a single
// library scan:
//
//  1. Report both the old and new paths, so Jellyfin rescans just those
//     folders rather than walking every library root.
//  2. Wait for the item to reappear at its new path, re-resolving its ID.
//     Jellyfin hashes an item's path into its ID, so the move necessarily
//     produced a *different* item; any ID held from before the move now
//     refers to something that no longer exists.
//  3. Refresh that newly-resolved ID with FullRefresh + replace, the only
//     mode that displaces artwork records still pointing at the old tier.
//
// Returns an error naming every item it could not refresh, so the caller
// can fall back to a whole-library scan. Items that did get refreshed stay
// refreshed - a partial failure is never rolled back.
func (c *Client) NotifyMoved(items []MovedItem) error {
	var oldPaths, newPaths []string
	pending := map[string]MovedItem{}
	for _, it := range items {
		if it.NewPath == "" {
			continue
		}
		if it.OldPath != "" {
			oldPaths = append(oldPaths, it.OldPath)
		}
		newPaths = append(newPaths, it.NewPath)
		pending[filepath.Clean(it.NewPath)] = it
	}
	if len(pending) == 0 {
		return nil
	}

	// Best effort: the poll below is what actually establishes the item
	// exists, so a failure to hand Jellyfin a hint isn't fatal on its own.
	if err := c.ReportMediaUpdated(oldPaths, "Deleted"); err != nil {
		c.logf("jellyfin: reporting vacated paths failed, continuing: %v", err)
	}
	if err := c.ReportMediaUpdated(newPaths, "Created"); err != nil {
		c.logf("jellyfin: reporting new paths failed, continuing: %v", err)
	}

	interval := c.ResolvePollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	timeout := c.ResolveTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}

	failures := map[string]error{}
	deadline := time.Now().Add(timeout)
	for {
		// One library snapshot per round, matched against every
		// outstanding item - resolving them one at a time would re-list
		// the entire library, for every user, once per item per round.
		ids, err := c.LibraryItemIDs()
		if err != nil {
			c.logf("jellyfin: listing items to resolve moved paths failed: %v", err)
		} else {
			for path, item := range pending {
				id, ok := ids[path]
				if !ok {
					continue
				}
				c.logf("jellyfin: resolved %q at %s to item %s, refreshing", item.Title, path, id)
				if err := c.RefreshItem(id, FullRefreshOptions()); err != nil {
					if errors.Is(err, ErrItemNotFound) {
						// Resolved and then vanished: the listing was
						// stale. Leave it pending for the next round.
						continue
					}
					failures[item.Title] = err
				}
				delete(pending, path)
			}
		}

		if len(pending) == 0 {
			break
		}
		if !time.Now().Add(interval).Before(deadline) {
			break
		}
		time.Sleep(interval)
	}

	for _, item := range pending {
		failures[item.Title] = fmt.Errorf("no Jellyfin item appeared at %s within %s", item.NewPath, timeout)
	}
	if len(failures) == 0 {
		return nil
	}

	titles := make([]string, 0, len(failures))
	for title := range failures {
		titles = append(titles, fmt.Sprintf("%s (%v)", title, failures[title]))
	}
	sort.Strings(titles)
	return fmt.Errorf("could not refresh %d item(s) in Jellyfin: %s", len(failures), strings.Join(titles, "; "))
}

// ServerID returns this Jellyfin server's own ID, needed for the
// "serverId" query parameter on a web UI deep link.
func (c *Client) ServerID() (string, error) {
	body, err := c.get("/System/Info", nil)
	if err != nil {
		return "", err
	}

	var info struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("GET /System/Info: decoding response: %w", err)
	}
	return info.ID, nil
}
