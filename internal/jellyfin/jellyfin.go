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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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

type itemsResponse struct {
	Items []struct {
		Path string `json:"Path"`
	} `json:"Items"`
}

// FavoritePaths returns the cleaned filesystem paths of every movie and
// series marked as a Favorite by any Jellyfin user, for matching back to
// Radarr/Sonarr items by path. Favorite status is per-user in Jellyfin's
// API (there's no library-wide "is this a favorite" flag), so this
// enumerates every user and unions their favorites - if anyone in the
// household favorited it, Coldarr treats it as protected.
func (c *Client) FavoritePaths() (map[string]bool, error) {
	body, err := c.get("/Users", nil)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}

	var users []userResource
	if err := json.Unmarshal(body, &users); err != nil {
		return nil, fmt.Errorf("listing users: decoding response: %w", err)
	}

	q := url.Values{}
	q.Set("Recursive", "true")
	q.Set("Filters", "IsFavorite")
	q.Set("IncludeItemTypes", "Movie,Series")
	q.Set("Fields", "Path")

	paths := map[string]bool{}
	for _, u := range users {
		body, err := c.get("/Users/"+u.ID+"/Items", q)
		if err != nil {
			return nil, fmt.Errorf("listing favorites for user %s: %w", u.ID, err)
		}

		var resp itemsResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("listing favorites for user %s: decoding response: %w", u.ID, err)
		}

		for _, item := range resp.Items {
			if item.Path == "" {
				continue
			}
			paths[filepath.Clean(item.Path)] = true
		}
	}
	return paths, nil
}
