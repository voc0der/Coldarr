// Package jellyfin sends a library refresh request after Coldarr moves
// media, reads Favorite status (matched back to Radarr/Sonarr items by
// path) so favorited items are never moved, and confirms connectivity.
// Jellyfin is a consumer of the library, never the mover.
package jellyfin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
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

// RefreshLibrary triggers a full library scan.
func (c *Client) RefreshLibrary() error {
	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/Library/Refresh", nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("X-Emby-Token", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST /Library/Refresh: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST /Library/Refresh: unexpected status %d", resp.StatusCode)
	}
	return nil
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
