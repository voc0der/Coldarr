// Package qbit is a small client for qBittorrent's WebUI API (v2) - just
// enough to inventory torrents, read their labels and trackers, and ask
// qBittorrent to move, relabel, or remove them. Like internal/arrapi it
// holds no policy: what Coldarr should do with a torrent is decided in
// internal/torrents. And like Radarr/Sonarr, qBittorrent does the moving
// itself, so its own record of where each torrent lives never drifts from
// what's on disk.
//
// Authentication comes in three shapes, all optional: an API key
// (qBittorrent 5.2+), a username and password, or neither, for a WebUI
// set to skip authentication for Coldarr's address. Sessions are cookies
// whose name changed in 5.2 (SID, then QBT_SID_<port>), so the client keeps
// a cookie jar rather than looking for one by name.
package qbit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/connectionhttp"
)

// Auth is how Coldarr signs in to qBittorrent. An API key wins when set;
// a username and password are the fallback, and are also used if
// qBittorrent turns the key down (a version older than 5.2 ignores it).
type Auth struct {
	APIKey   string
	Username string
	Password string
}

type Client struct {
	baseURL string
	auth    Auth
	http    *http.Client

	// mu serializes signing in, and guards the two facts below: callers
	// may share one client across goroutines.
	mu sync.Mutex
	// useKey is cleared once qBittorrent has refused the API key, so later
	// requests go straight to the cookie session instead of failing first.
	useKey bool
	// loggedIn records a successful login, so the cookie session is reused
	// rather than re-established for every request.
	loggedIn bool
}

func NewClient(baseURL string, auth Auth) *Client {
	jar, _ := cookiejar.New(nil) // only fails for non-nil options
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		auth:    auth,
		http:    &http.Client{Timeout: 30 * time.Second, Jar: jar, Transport: connectionhttp.Transport},
		useKey:  auth.APIKey != "",
	}
}

// StatusError is returned when qBittorrent answers a request with a non-2xx
// status, so callers can tell a refusal (qBittorrent decided not to do
// something) from a request that may never have arrived.
type StatusError struct {
	Method, Path string
	Code         int
	Body         string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: unexpected status %d: %s", e.Method, e.Path, e.Code, truncate(strings.TrimSpace(e.Body), 300))
}

// ErrNotFound is returned when qBittorrent no longer has the torrent a
// request named - typically removed between listing and asking about it.
var ErrNotFound = errors.New("qbittorrent: torrent not found")

// AuthError means qBittorrent would not let Coldarr in. It is never retried
// on its own: repeating a rejected login is how an address gets banned.
type AuthError struct {
	Reason string
}

func (e *AuthError) Error() string {
	return "qBittorrent: " + e.Reason
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// send performs one HTTP exchange and returns the body and status code,
// leaving status handling to its callers. form, when non-nil, makes the
// request a form POST - the only kind of write qBittorrent accepts.
func (c *Client) send(path string, query, form url.Values, withKey bool) ([]byte, int, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	method := http.MethodGet
	var body io.Reader
	if form != nil {
		method = http.MethodPost
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, 0, fmt.Errorf("building request: %w", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// No Origin or Referer: qBittorrent only runs its cross-site check
	// when one of them is present, and a proxy rewriting Host would make
	// an honest one look forged.
	if withKey {
		req.Header.Set("X-API-Key", c.auth.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: reading response: %w", method, path, err)
	}
	return respBody, resp.StatusCode, nil
}

// do sends a request, signing in first when the client has a username and
// no working API key, and signing in again once if qBittorrent says the
// session has lapsed. It returns the body of a 2xx response; anything else
// is an error.
func (c *Client) do(path string, query, form url.Values) ([]byte, error) {
	withKey, err := c.prepare()
	if err != nil {
		return nil, err
	}

	body, status, err := c.send(path, query, form, withKey)
	if err != nil {
		return nil, err
	}

	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		if c.auth.Username == "" {
			if withKey {
				return nil, &AuthError{Reason: "it did not accept the API key (keys need qBittorrent 5.2 or later) - check the key, or set a username and password instead"}
			}
			return nil, &AuthError{Reason: "it needs a login - set a username and password (or an API key, on qBittorrent 5.2+), unless its WebUI is set to skip authentication for Coldarr's address"}
		}
		if err := c.relogin(); err != nil {
			return nil, err
		}
		body, status, err = c.send(path, query, form, false)
		if err != nil {
			return nil, err
		}
	}

	method := http.MethodGet
	if form != nil {
		method = http.MethodPost
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	if status < 200 || status >= 300 {
		return nil, &StatusError{Method: method, Path: path, Code: status, Body: string(body)}
	}
	return body, nil
}

// prepare reports whether the next request should carry the API key, and
// signs in first if it won't and the client has a login to use.
func (c *Client) prepare() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.useKey {
		return true, nil
	}
	if c.auth.Username != "" && !c.loggedIn {
		if err := c.loginLocked(); err != nil {
			return false, err
		}
	}
	return false, nil
}

// relogin drops a session qBittorrent has stopped honoring - or an API key
// it won't take - and signs in with the username and password instead.
func (c *Client) relogin() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.useKey = false
	c.loggedIn = false
	return c.loginLocked()
}

// loginLocked must be called with mu held. qBittorrent answers a wrong
// password with 200 and the body "Fails." (newer builds may use 401), and
// a banned address with 403.
func (c *Client) loginLocked() error {
	form := url.Values{"username": {c.auth.Username}, "password": {c.auth.Password}}
	body, status, err := c.send("/api/v2/auth/login", nil, form, false)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(string(body))
	switch {
	case status == http.StatusForbidden:
		return &AuthError{Reason: "it has temporarily banned Coldarr's address after too many failed logins" + bodySuffix(text)}
	case status == http.StatusUnauthorized, status >= 200 && status < 300 && text == "Fails.":
		return &AuthError{Reason: "it rejected the username or password"}
	case status < 200 || status >= 300:
		return &StatusError{Method: http.MethodPost, Path: "/api/v2/auth/login", Code: status, Body: text}
	}
	c.loggedIn = true
	return nil
}

func bodySuffix(text string) string {
	if text == "" {
		return ""
	}
	return ": " + truncate(text, 200)
}

func (c *Client) getJSON(path string, query url.Values, out any) error {
	body, err := c.do(path, query, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		trimmed := strings.TrimSpace(string(body))
		if strings.HasPrefix(trimmed, "<") {
			return fmt.Errorf("GET %s: qBittorrent sent back a web page instead of data - is the URL pointing at qBittorrent's WebUI, and not a login page in front of it?", path)
		}
		return fmt.Errorf("GET %s: decoding response: %w", path, err)
	}
	return nil
}

func (c *Client) post(path string, form url.Values) error {
	_, err := c.do(path, nil, form)
	return err
}

// Version returns the qBittorrent version (e.g. "v5.0.4") - the connection
// test, and the cheapest call that proves the login works.
func (c *Client) Version() (string, error) {
	body, err := c.do("/api/v2/app/version", nil, nil)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(body))
	if strings.HasPrefix(version, "<") {
		return "", fmt.Errorf("GET /api/v2/app/version: qBittorrent sent back a web page instead of a version - is the URL pointing at qBittorrent's WebUI, and not a login page in front of it?")
	}
	return version, nil
}

// Torrents returns every torrent qBittorrent has.
func (c *Client) Torrents() ([]Torrent, error) {
	return c.torrents(nil)
}

// TorrentsByHash returns just the named torrents - those qBittorrent still
// has; a missing hash is simply absent from the result.
func (c *Client) TorrentsByHash(hashes []string) ([]Torrent, error) {
	if len(hashes) == 0 {
		return nil, nil
	}
	return c.torrents(url.Values{"hashes": {strings.Join(hashes, "|")}})
}

func (c *Client) torrents(query url.Values) ([]Torrent, error) {
	var raw []torrentResource
	if err := c.getJSON("/api/v2/torrents/info", query, &raw); err != nil {
		return nil, err
	}
	out := make([]Torrent, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.normalize())
	}
	return out, nil
}

// Category is one of qBittorrent's categories.
type Category struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

// Categories returns qBittorrent's categories, keyed by name.
func (c *Client) Categories() (map[string]Category, error) {
	out := map[string]Category{}
	if err := c.getJSON("/api/v2/torrents/categories", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Tags returns every tag qBittorrent knows about.
func (c *Client) Tags() ([]string, error) {
	var out []string
	if err := c.getJSON("/api/v2/torrents/tags", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type trackerResource struct {
	URL string `json:"url"`
}

// TrackerHosts returns the host of every tracker the torrent announces to,
// lower-cased and de-duplicated. Only hosts ever leave this function: an
// announce URL usually carries the account's passkey. ErrNotFound means
// qBittorrent no longer has the torrent.
func (c *Client) TrackerHosts(hash string) ([]string, error) {
	var raw []trackerResource
	if err := c.getJSON("/api/v2/torrents/trackers", url.Values{"hash": {hash}}, &raw); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	hosts := []string{}
	for _, t := range raw {
		host := TrackerHost(t.URL)
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// TrackerHost returns the lower-cased host of a tracker announce URL, or ""
// for anything that isn't one - qBittorrent lists DHT, PeX and LSD as
// pseudo-trackers named like "** [DHT] **".
func TrackerHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "**") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

type fileResource struct {
	Name string `json:"name"`
}

// FileNames returns the path of each of the torrent's files, relative to its
// save path. ErrNotFound means qBittorrent no longer has the torrent.
func (c *Client) FileNames(hash string) ([]string, error) {
	var raw []fileResource
	if err := c.getJSON("/api/v2/torrents/files", url.Values{"hash": {hash}}, &raw); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(raw))
	for _, f := range raw {
		if f.Name != "" {
			names = append(names, f.Name)
		}
	}
	return names, nil
}

// SetLocation asks qBittorrent to move the torrents' data to location. It
// returns once qBittorrent has accepted the move; the copy itself carries
// on inside qBittorrent, state "moving", and survives Coldarr restarting.
func (c *Client) SetLocation(hashes []string, location string) error {
	return c.post("/api/v2/torrents/setLocation", url.Values{"hashes": {joinHashes(hashes)}, "location": {location}})
}

// SetCategory changes the torrents' category, which must already exist -
// qBittorrent answers 409 otherwise (see CreateCategory).
func (c *Client) SetCategory(hashes []string, category string) error {
	return c.post("/api/v2/torrents/setCategory", url.Values{"hashes": {joinHashes(hashes)}, "category": {category}})
}

// CreateCategory adds a category with no save path of its own, so assigning
// it to a torrent never moves that torrent's data by itself.
func (c *Client) CreateCategory(name string) error {
	return c.post("/api/v2/torrents/createCategory", url.Values{"category": {name}, "savePath": {""}})
}

// SetAutoManagement switches Automatic Torrent Management on or off.
func (c *Client) SetAutoManagement(hashes []string, enable bool) error {
	return c.post("/api/v2/torrents/setAutoManagement", url.Values{"hashes": {joinHashes(hashes)}, "enable": {fmt.Sprint(enable)}})
}

// Delete removes the torrents from qBittorrent, and their data with them
// when deleteFiles is true. qBittorrent answers 200 whatever happened, so
// callers confirm by listing the torrents again.
func (c *Client) Delete(hashes []string, deleteFiles bool) error {
	return c.post("/api/v2/torrents/delete", url.Values{"hashes": {joinHashes(hashes)}, "deleteFiles": {fmt.Sprint(deleteFiles)}})
}

func joinHashes(hashes []string) string {
	return strings.Join(hashes, "|")
}
