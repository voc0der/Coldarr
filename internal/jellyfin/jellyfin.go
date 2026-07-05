// Package jellyfin sends a library refresh request after Coldarr moves
// media, so playback paths stay in sync. Jellyfin is a consumer of the
// library, never the mover.
package jellyfin

import (
	"fmt"
	"net/http"
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
