// Package arrapi is a small client for the Radarr and Sonarr (Servarr) v3
// REST APIs - just enough to inventory library items, check for active
// downloads, and perform bulk root-folder moves.
package arrapi

import (
	"bytes"
	"encoding/json"
	"errors"
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

func (c *client) post(path string, body interface{}, out interface{}) error {
	return c.do(http.MethodPost, path, nil, body, out)
}

// StatusError is returned when a request reaches the server but gets back
// a non-2xx response, so callers can distinguish "this item doesn't exist"
// (404) from a genuine connectivity/auth failure.
type StatusError struct {
	Method, Path string
	Code         int
	Body         string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: unexpected status %d: %s", e.Method, e.Path, e.Code, truncate(e.Body, 500))
}

// IsNotFound reports whether err is a StatusError for a 404 response -
// Radarr/Sonarr return this when asked about an item ID they no longer
// know about (e.g. deleted from the library since Coldarr moved it).
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
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
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Method: method, Path: path, Code: resp.StatusCode, Body: string(respBody)}
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("%s %s: decoding response: %w", method, path, err)
		}
	}

	return nil
}

// commandResource is the subset of Radarr/Sonarr's command-queue resource
// (POST/GET /api/v3/command) that callers need to trigger a command and
// poll it to completion, plus Name for recognizing what a listed command
// actually is (see activeMoveCommands).
type commandResource struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// activeMoveCommands returns how many move-type commands Radarr/Sonarr is
// currently executing or has queued. Bulk root-folder relocations
// (movie/editor, series/editor with moveFiles) run as commands in this
// queue - NOT as records in /api/v3/queue, which only tracks
// downloads/imports - so this, not the download queue, is the only place
// an in-flight move is visible from outside. Critically, these commands
// keep running inside Radarr/Sonarr even if the process that requested
// them (Coldarr) has since crashed or restarted, so callers use this to
// detect "a previous run's moves are still physically copying" before
// trusting disk numbers or starting new moves. Matched by name substring
// rather than an exact allow-list so renamed/added move-type commands in
// future Radarr/Sonarr versions fail toward caution (a false positive
// merely delays an apply; a false negative overlaps writes).
func (c *client) activeMoveCommands() (int, error) {
	var cmds []commandResource
	if err := c.get("/api/v3/command", nil, &cmds); err != nil {
		return 0, err
	}
	n := 0
	for _, cmd := range cmds {
		if cmd.Status != "queued" && cmd.Status != "started" {
			continue
		}
		if strings.Contains(strings.ToLower(cmd.Name), "move") {
			n++
		}
	}
	return n, nil
}

// runCommand starts a Radarr/Sonarr command (e.g. a folder rescan) and
// blocks until it reaches a terminal state, so callers see the effect of
// the command (an updated database record) rather than racing a
// still-running background job. body must include the command's "name"
// field alongside whatever arguments it needs (e.g. {"name": "RescanMovie",
// "movieId": 5}) - see each app's MediaFiles/Commands/*.cs for the exact
// shape, since it varies per command.
func (c *client) runCommand(body interface{}) error {
	var cmd commandResource
	if err := c.post("/api/v3/command", body, &cmd); err != nil {
		return err
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := c.get(fmt.Sprintf("/api/v3/command/%d", cmd.ID), nil, &cmd); err != nil {
			return err
		}
		switch cmd.Status {
		case "completed":
			return nil
		case "failed", "aborted", "cancelled", "orphaned":
			return fmt.Errorf("command %d ended with status %q", cmd.ID, cmd.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("command %d did not finish within 2m (last status %q)", cmd.ID, cmd.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type systemStatusResource struct {
	Version string `json:"version"`
}

// ping hits the Servarr system status endpoint, common to both Radarr and
// Sonarr, and returns the reported version - used to confirm a connection
// actually works before saving it.
func (c *client) ping() (version string, err error) {
	var status systemStatusResource
	if err := c.get("/api/v3/system/status", nil, &status); err != nil {
		return "", err
	}
	return status.Version, nil
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
