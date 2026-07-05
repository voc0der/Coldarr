// Package arrapi is a small client for the Radarr and Sonarr (Servarr) v3
// REST APIs - just enough to inventory library items, check for active
// downloads, and perform bulk root-folder moves.
package arrapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func newClient(baseURL, apiKey string) *client {
	return &client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *client) get(path string, query url.Values, out interface{}) error {
	return c.do(http.MethodGet, path, query, nil, out)
}

func (c *client) put(path string, body interface{}, out interface{}) error {
	return c.do(http.MethodPut, path, nil, body, out)
}

func (c *client) do(method, path string, query url.Values, body interface{}, out interface{}) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, u, reqBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: unexpected status %d: %s", method, path, resp.StatusCode, truncate(string(respBody), 500))
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
		}
	}

	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type tagResource struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

type qualityProfileResource struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func tagLabels(ids []int, byID map[int]string) []string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		if label, ok := byID[id]; ok {
			labels = append(labels, label)
		}
	}
	return labels
}
