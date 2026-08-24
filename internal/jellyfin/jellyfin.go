// Package jellyfin re-points Jellyfin at media Coldarr has moved between
// tiers (see ReportMoved and ResolveAndRefresh), reads Favorite status
// (matched back to Radarr/Sonarr items by path) so favorited items are
// kept on hot storage, and confirms connectivity. Jellyfin is a consumer
// of the library, never the mover.
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

	// ResolvePollInterval and ResolveTimeout bound how long
	// ResolveAndRefresh waits for Jellyfin to notice a moved item at its
	// new path. Jellyfin debounces filesystem-change reports before acting
	// on them, so the new item is never visible immediately after the move
	// lands.
	ResolvePollInterval time.Duration
	ResolveTimeout      time.Duration
}

const (
	// DefaultResolvePollInterval is how long ResolveAndRefresh waits before
	// its second look at the library. Subsequent waits back off from here
	// (see maxResolvePollInterval).
	DefaultResolvePollInterval = 10 * time.Second

	// DefaultResolveTimeout budgets for Jellyfin actually doing the work a
	// reported path asks for, which is far more than the report costs.
	// Jellyfin sits on a filesystem-change report for LibraryMonitorDelay
	// (60s out of the box) before touching it, and a path it has never seen
	// before then resolves up to its containing library root and
	// re-validates that root - minutes on a large library, serialized
	// against every other root in the same batch.
	//
	// Items reported as they landed (see ReportMoved) have usually been
	// indexed long before this matters; the budget exists for the tail of a
	// run, where the last item lands with no head start at all.
	DefaultResolveTimeout = 15 * time.Minute

	// maxResolvePollInterval caps the backoff between polls. Each poll
	// lists every movie and series in the library, once per Jellyfin user,
	// so polling on a fixed short interval for the whole timeout would put
	// its heaviest read load on a server that is, by construction, busy
	// running the very scan being waited for.
	maxResolvePollInterval = 60 * time.Second

	// UserDataRestoreTaskKey is the stable IScheduledTask key published by
	// the Restore User Data After Move plugin. Jellyfin's task ID is a
	// separate runtime value, so callers must resolve this key before each
	// launch rather than guessing an endpoint ID.
	UserDataRestoreTaskKey = "UserDataRestore"
)

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL:             strings.TrimRight(baseURL, "/"),
		apiKey:              apiKey,
		http:                &http.Client{Timeout: 30 * time.Second},
		Logf:                log.Printf,
		ResolvePollInterval: DefaultResolvePollInterval,
		ResolveTimeout:      DefaultResolveTimeout,
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// Client identity sent in the Authorization header. Only the token
// authorizes; these fields are metadata, but they're what Jellyfin writes
// to its logs and shows under Dashboard -> Devices, so an anonymous entry
// there is worth avoiding when something needs tracing back to Coldarr.
const (
	clientName = "Coldarr"
	deviceID   = "coldarr"
)

// Version is stamped into the Authorization header as the client version.
// main sets it from the build-time version at startup; "dev" otherwise.
var Version = "dev"

// authorize applies Jellyfin's authorization header to req. The
// MediaBrowser scheme is the only credential sent, and the only one
// accepted across both the 10.x line and 12.0, which defaults legacy
// authorization off.
func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", fmt.Sprintf(
		"MediaBrowser Client=%q, Device=%q, DeviceId=%q, Version=%q, Token=%q",
		clientName, clientName, deviceID, Version, c.apiKey,
	))
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
	c.authorize(req)

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
	c.authorize(req)
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

// StartScheduledTask starts one Jellyfin scheduled task by its stable key.
// Jellyfin's start endpoint accepts the task's runtime ID, while plugin code
// exposes a stable Key, so this first resolves key -> ID from /ScheduledTasks
// and then issues exactly one start request. An already-running task needs no
// second start and is treated as success.
func (c *Client) StartScheduledTask(key string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("scheduled task key is required")
	}

	body, err := c.get("/ScheduledTasks", nil)
	if err != nil {
		return fmt.Errorf("listing scheduled tasks: %w", err)
	}
	var tasks []struct {
		ID    string `json:"Id"`
		Key   string `json:"Key"`
		State string `json:"State"`
	}
	if err := json.Unmarshal(body, &tasks); err != nil {
		return fmt.Errorf("GET /ScheduledTasks: decoding response: %w", err)
	}

	for _, task := range tasks {
		if !strings.EqualFold(task.Key, key) {
			continue
		}
		if task.ID == "" {
			return fmt.Errorf("scheduled task %q has no runtime ID", key)
		}
		if strings.EqualFold(task.State, "Running") {
			return nil
		}
		return c.post("/ScheduledTasks/Running/"+url.PathEscape(task.ID), nil, nil)
	}

	return fmt.Errorf("scheduled task %q not found (required plugin is not installed or loaded)", key)
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
// household favorited it, Coldarr keeps it on hot storage.
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

// ReportMoved tells Jellyfin which folders items have just vacated and
// occupied, so it rescans those rather than walking every library root.
//
// This is deliberately callable on its own, one item at a time, and that
// is how a mid-run caller should use it. Jellyfin does not act on a report
// when it arrives: it debounces for LibraryMonitorDelay (60s by default)
// and only then validates the affected folder, which for a path it has
// never seen before means resolving up to the containing library root and
// re-validating the whole root. Reporting each item the moment it lands
// puts that work in parallel with the rest of the run - moves take minutes
// to hours - instead of starting it cold after the last move finishes and
// then waiting on it. See ResolveAndRefresh for the other half.
//
// Reporting the same path twice is not free. Jellyfin folds a new report
// into the pending refresher already covering that path and restarts its
// timer, so a redundant second report pushes the rescan back by another
// LibraryMonitorDelay - delaying the very work the first report asked for.
// Callers that reported an item during the run must not report it again.
//
// Errors are worth returning rather than swallowing: ResolveAndRefresh is
// what ultimately establishes the item exists, so a failed report is not
// fatal, but it does mean this item got no head start and the caller may
// want to retry it later.
func (c *Client) ReportMoved(items []MovedItem) error {
	var oldPaths, newPaths []string
	for _, it := range items {
		if it.NewPath == "" {
			continue
		}
		if it.OldPath != "" {
			oldPaths = append(oldPaths, it.OldPath)
		}
		newPaths = append(newPaths, it.NewPath)
		c.logf("jellyfin: reporting move %s -> %s", it.OldPath, it.NewPath)
	}
	if len(newPaths) == 0 {
		return nil
	}

	var errs []error
	if err := c.ReportMediaUpdated(oldPaths, "Deleted"); err != nil {
		errs = append(errs, fmt.Errorf("reporting vacated paths: %w", err))
	}
	if err := c.ReportMediaUpdated(newPaths, "Created"); err != nil {
		errs = append(errs, fmt.Errorf("reporting new paths: %w", err))
	}
	return errors.Join(errs...)
}

// resolveBackoffCeiling returns how far ResolveAndRefresh may back its
// poll interval off from the configured base.
//
// Floored at base, because backoff must only ever slow polling down.
// maxResolvePollInterval is there to stop a short interval from hammering
// a server that's mid-scan; an operator who configures an interval above
// it is asking for even less polling pressure, so folding their interval
// into the cap unconditionally would silently poll more often than they
// asked for - the opposite of what the knob is for, aimed at exactly the
// large-library setup most likely to be reaching for it.
func resolveBackoffCeiling(base time.Duration) time.Duration {
	return max(base, min(8*base, maxResolvePollInterval))
}

// ResolveAndRefresh waits for each moved item to reappear at its new path
// and then refreshes it, which is the reason a moved item keeps its
// artwork. It does not report any paths itself - see ReportMoved, which
// the caller is expected to have already done, ideally per item as each
// move landed.
//
// Re-resolving is not optional, and cannot be collapsed into a library
// scan. Jellyfin hashes an item's path into its ID, so the move
// necessarily produced a *different* item; any ID held from before the
// move now refers to something that no longer exists. And only FullRefresh
// + replace displaces artwork records still pointing at the old tier (see
// FullRefreshOptions), which needs that new ID.
//
// Returns an error naming every item it could not refresh, so the caller
// can fall back to a whole-library scan. Items that did get refreshed stay
// refreshed - a partial failure is never rolled back.
func (c *Client) ResolveAndRefresh(items []MovedItem) error {
	pending := map[string]MovedItem{}
	for _, it := range items {
		if it.NewPath == "" {
			continue
		}
		pending[filepath.Clean(it.NewPath)] = it
	}
	if len(pending) == 0 {
		return nil
	}

	interval := c.ResolvePollInterval
	if interval <= 0 {
		interval = DefaultResolvePollInterval
	}
	timeout := c.ResolveTimeout
	if timeout <= 0 {
		timeout = DefaultResolveTimeout
	}
	maxInterval := resolveBackoffCeiling(interval)

	// Keyed by the item's new path, the same key space as pending, rather
	// than by title: two items can legitimately share a title (a remake, or
	// the same show tracked in two libraries), and keying by it would
	// collapse them into one entry - under-reporting both the count and
	// which items were actually left with stale artwork.
	failures := map[string]error{}
	deadline := time.Now().Add(timeout)
	wait := interval
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
					failures[path] = fmt.Errorf("%s: %w", item.Title, err)
				}
				delete(pending, path)
			}
		}

		if len(pending) == 0 {
			break
		}

		// Clamp the sleep to what's left rather than breaking out early on
		// a backed-off interval that overshoots the deadline: a poll that
		// still fits inside the budget is always worth taking, so the last
		// one lands right at it. Clamping `sleep` and not `wait` keeps that
		// from also shortening the interval every round after it.
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		sleep := min(wait, remaining)
		c.logf("jellyfin: %d item(s) not yet visible at their new path, re-checking in %s", len(pending), sleep.Round(time.Second))
		time.Sleep(sleep)
		wait = min(2*wait, maxInterval)
	}

	for path, item := range pending {
		failures[path] = fmt.Errorf("%s: no Jellyfin item appeared at %s within %s", item.Title, item.NewPath, timeout)
	}
	if len(failures) == 0 {
		return nil
	}

	msgs := make([]string, 0, len(failures))
	for _, err := range failures {
		msgs = append(msgs, err.Error())
	}
	sort.Strings(msgs)
	return fmt.Errorf("could not refresh %d item(s) in Jellyfin: %s", len(failures), strings.Join(msgs, "; "))
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
