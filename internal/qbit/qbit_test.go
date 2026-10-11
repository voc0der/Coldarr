package qbit

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeQbit is the slice of qBittorrent's WebUI API the client uses, with a
// real login: a cookie session named the way qBittorrent 5.2+ names it, an
// optional API key, and the "Fails." answer a wrong password gets.
type fakeQbit struct {
	*httptest.Server

	mu       sync.Mutex
	username string
	password string
	apiKey   string // "" means the server predates API keys and ignores them
	noAuth   bool   // the WebUI skips authentication for this client
	banned   bool
	sessions map[string]bool
	logins   int
	posts    []postedForm
	torrents string // raw /torrents/info body
	trackers map[string]string
}

type postedForm struct {
	Path string
	Form map[string]string
}

func newFakeQbit(t *testing.T) *fakeQbit {
	t.Helper()
	f := &fakeQbit{username: "admin", password: "secret", sessions: map[string]bool{}, torrents: "[]", trackers: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeQbit) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/api/v2/auth/login" {
		f.logins++
		_ = r.ParseForm()
		switch {
		case f.banned:
			http.Error(w, "Your IP address has been banned after too many failed authentication attempts.", http.StatusForbidden)
		case r.Form.Get("username") == f.username && r.Form.Get("password") == f.password:
			sid := "session-" + strconv.Itoa(f.logins)
			f.sessions[sid] = true
			http.SetCookie(w, &http.Cookie{Name: "QBT_SID_8080", Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}) //nolint:gosec // qBittorrent over plain HTTP sets no Secure flag, and this fakes it
			_, _ = w.Write([]byte("Ok."))
		default:
			_, _ = w.Write([]byte("Fails."))
		}
		return
	}

	if !f.authorized(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		f.posts = append(f.posts, postedForm{Path: r.URL.Path, Form: form})
	}

	switch r.URL.Path {
	case "/api/v2/app/version":
		_, _ = w.Write([]byte("v5.2.3"))
	case "/api/v2/torrents/info":
		_, _ = w.Write([]byte(f.torrents))
	case "/api/v2/torrents/categories":
		_, _ = w.Write([]byte(`{"movies": {"name": "movies", "savePath": "/downloads/movies"}, "seed": {"name": "seed", "savePath": ""}}`))
	case "/api/v2/torrents/tags":
		_, _ = w.Write([]byte(`["transfer", "prowlarr"]`))
	case "/api/v2/torrents/trackers":
		body, ok := f.trackers[r.URL.Query().Get("hash")]
		if !ok {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	case "/api/v2/torrents/setCategory":
		if r.PostForm.Get("category") == "missing" {
			http.Error(w, "Incorrect category name", http.StatusConflict)
		}
	case "/api/v2/torrents/setLocation", "/api/v2/torrents/createCategory", "/api/v2/torrents/setAutoManagement", "/api/v2/torrents/delete":
	default:
		http.NotFound(w, r)
	}
}

// authorized must be called with mu held.
func (f *fakeQbit) authorized(r *http.Request) bool {
	if f.noAuth {
		return true
	}
	if key := r.Header.Get("X-API-Key"); key != "" && f.apiKey != "" {
		return key == f.apiKey
	}
	cookie, err := r.Cookie("QBT_SID_8080")
	return err == nil && f.sessions[cookie.Value]
}

func (f *fakeQbit) expireSessions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = map[string]bool{}
}

func (f *fakeQbit) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logins
}

func (f *fakeQbit) posted() []postedForm {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]postedForm(nil), f.posts...)
}

func TestClient_LogsInOnceAndReusesTheSession(t *testing.T) {
	f := newFakeQbit(t)
	c := NewClient(f.URL+"/", Auth{Username: "admin", Password: "secret"})

	for range 3 {
		if v, err := c.Version(); err != nil || v != "v5.2.3" {
			t.Fatalf("Version() = %q, %v; want v5.2.3", v, err)
		}
	}
	if n := f.loginCount(); n != 1 {
		t.Errorf("logins = %d, want one session reused for every request", n)
	}
}

// TestClient_SignsInAgainWhenTheSessionLapses: qBittorrent forgets sessions
// on restart and after its session timeout; the client signs in once more
// rather than failing the request.
func TestClient_SignsInAgainWhenTheSessionLapses(t *testing.T) {
	f := newFakeQbit(t)
	c := NewClient(f.URL, Auth{Username: "admin", Password: "secret"})
	if _, err := c.Version(); err != nil {
		t.Fatal(err)
	}
	f.expireSessions()
	if _, err := c.Version(); err != nil {
		t.Fatalf("Version() after the session lapsed = %v, want a transparent re-login", err)
	}
	if n := f.loginCount(); n != 2 {
		t.Errorf("logins = %d, want 2", n)
	}
}

func TestClient_AuthFailuresAreClearAndNeverRetried(t *testing.T) {
	f := newFakeQbit(t)

	_, err := NewClient(f.URL, Auth{Username: "admin", Password: "wrong"}).Version()
	var authErr *AuthError
	if !errors.As(err, &authErr) || !strings.Contains(err.Error(), "rejected the username or password") {
		t.Fatalf("wrong password: err = %v, want an AuthError saying so", err)
	}
	if n := f.loginCount(); n != 1 {
		t.Errorf("logins = %d, want exactly one attempt - repeating a rejected login is how addresses get banned", n)
	}

	_, err = NewClient(f.URL, Auth{}).Version()
	if !errors.As(err, &authErr) || !strings.Contains(err.Error(), "needs a login") {
		t.Errorf("no credentials: err = %v, want an AuthError asking for a login", err)
	}

	f.mu.Lock()
	f.banned = true
	f.mu.Unlock()
	_, err = NewClient(f.URL, Auth{Username: "admin", Password: "secret"}).Version()
	if !errors.As(err, &authErr) || !strings.Contains(err.Error(), "banned") {
		t.Errorf("banned: err = %v, want an AuthError about the ban", err)
	}
}

func TestClient_APIKey(t *testing.T) {
	f := newFakeQbit(t)
	f.apiKey = "key-123"

	if _, err := NewClient(f.URL, Auth{APIKey: "key-123"}).Version(); err != nil {
		t.Fatalf("API key: %v", err)
	}
	if n := f.loginCount(); n != 0 {
		t.Errorf("logins = %d, want none when an API key works", n)
	}

	_, err := NewClient(f.URL, Auth{APIKey: "wrong"}).Version()
	if err == nil || !strings.Contains(err.Error(), "did not accept the API key") {
		t.Errorf("wrong key: err = %v, want it called out", err)
	}

	// A key qBittorrent turns down falls back to the username and
	// password, and later requests skip the key entirely.
	c := NewClient(f.URL, Auth{APIKey: "wrong", Username: "admin", Password: "secret"})
	for range 2 {
		if _, err := c.Version(); err != nil {
			t.Fatalf("key with login fallback: %v", err)
		}
	}
	if n := f.loginCount(); n != 1 {
		t.Errorf("logins = %d, want a single fallback login", n)
	}
}

func TestClient_NoAuthNeededWhenTheWebUIBypassesIt(t *testing.T) {
	f := newFakeQbit(t)
	f.noAuth = true
	if _, err := NewClient(f.URL, Auth{}).Version(); err != nil {
		t.Fatalf("auth bypass: %v", err)
	}
}

func TestClient_TransportAndShapeFailures(t *testing.T) {
	// Unreachable.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if _, err := NewClient(dead.URL, Auth{}).Torrents(); err == nil {
		t.Error("an unreachable qBittorrent must be an error")
	}

	// A login page in front of qBittorrent (an auth proxy) answering 200.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><html><body>Sign in</body></html>"))
	}))
	defer html.Close()
	if _, err := NewClient(html.URL, Auth{}).Torrents(); err == nil || !strings.Contains(err.Error(), "web page instead of data") {
		t.Errorf("HTML body: err = %v, want it identified as a web page", err)
	}
	if _, err := NewClient(html.URL, Auth{}).Version(); err == nil || !strings.Contains(err.Error(), "web page") {
		t.Errorf("HTML version: err = %v, want it identified as a web page", err)
	}

	// An API whose shape changed underneath Coldarr.
	changed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"torrents": []}`))
	}))
	defer changed.Close()
	if _, err := NewClient(changed.URL, Auth{}).Torrents(); err == nil || !strings.Contains(err.Error(), "decoding response") {
		t.Errorf("changed shape: err = %v, want a decoding error", err)
	}

	// Server errors keep their status.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	var statusErr *StatusError
	if err := NewClient(broken.URL, Auth{}).SetLocation([]string{"a"}, "/seed"); !errors.As(err, &statusErr) || statusErr.Code != http.StatusInternalServerError {
		t.Errorf("500: err = %v, want a StatusError carrying the code", err)
	}
	if _, err := NewClient(broken.URL, Auth{Username: "u", Password: "p"}).Version(); !errors.As(err, &statusErr) {
		t.Errorf("500 on login: err = %v, want a StatusError", err)
	}
}

func TestTorrents_Normalized(t *testing.T) {
	f := newFakeQbit(t)
	f.noAuth = true
	f.torrents = `[
		{"hash": "ABCDEF", "name": "Movie.2020", "state": "stalledUP", "category": "movies", "tags": "seed, transfer ,",
		 "save_path": "/downloads", "content_path": "/downloads/Movie.2020", "added_on": 1700000000, "completion_on": 1700003600,
		 "seeding_time": 7200, "amount_left": 0, "size": 1000, "progress": 1, "auto_tmm": false,
		 "tracker": "https://Tracker.Example.org:443/announce/PASSKEY123", "ratio": 1.5},
		{"hash": "123", "name": "Downloading", "state": "downloading", "tags": ["a", " b "],
		 "completion_on": 4294967295, "progress": 0.4, "amount_left": 600, "size": 1000, "tracker": ""},
		{"hash": "456", "name": "Old qBittorrent", "state": "pausedUP", "tags": "", "completion_on": -1}
	]`
	got, err := NewClient(f.URL, Auth{}).Torrents()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d torrents, want 3", len(got))
	}

	a := got[0]
	if a.Hash != "abcdef" || a.TrackerHost != "tracker.example.org" || !a.SeedingTimeKnown || a.SeedingTime != 2*time.Hour {
		t.Errorf("first torrent = %+v; want a lower-case hash, the tracker host only, and 2h seeding", a)
	}
	if !a.CompletedOn.Equal(time.Unix(1700003600, 0)) || !a.Complete() {
		t.Errorf("first torrent completion = %v, complete %v", a.CompletedOn, a.Complete())
	}
	if strings.Join(a.Labels(), ",") != "movies,seed,transfer" {
		t.Errorf("labels = %v, want the category then each tag", a.Labels())
	}
	if strings.Contains(a.TrackerHost, "PASSKEY") {
		t.Fatal("the announce URL's passkey must never survive parsing")
	}

	b := got[1]
	if !b.CompletedOn.IsZero() || b.Complete() || strings.Join(b.Tags, ",") != "a,b" {
		t.Errorf("second torrent = %+v; want unset completion, incomplete, and array tags accepted", b)
	}
	if got[2].SeedingTimeKnown || len(got[2].Tags) != 0 || !got[2].CompletedOn.IsZero() {
		t.Errorf("third torrent = %+v; want unknown seeding time, no tags, unset completion", got[2])
	}
}

func TestTorrentsByHash(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("hashes")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, Auth{})
	if got, err := c.TorrentsByHash(nil); err != nil || got != nil {
		t.Fatalf("no hashes = %v, %v; want nothing asked", got, err)
	}
	if _, err := c.TorrentsByHash([]string{"a", "b"}); err != nil || gotQuery != "a|b" {
		t.Errorf("hashes query = %q, %v; want a|b", gotQuery, err)
	}
}

func TestLabelsAndTrackers(t *testing.T) {
	f := newFakeQbit(t)
	f.noAuth = true
	f.trackers["abc"] = `[
		{"url": "** [DHT] **", "status": 0, "tier": -1},
		{"url": "** [PeX] **", "status": 0, "tier": -1},
		{"url": "https://tracker.example.org/announce?passkey=SECRET", "status": 2, "tier": 0},
		{"url": "udp://Tracker.Example.org:1337/announce", "status": 2, "tier": 1},
		{"url": "http://backup.example.net/announce", "status": 4, "tier": 2},
		{"url": "not a url", "status": 4, "tier": 3}
	]`
	c := NewClient(f.URL, Auth{})

	cats, err := c.Categories()
	if err != nil || cats["movies"].SavePath != "/downloads/movies" || len(cats) != 2 {
		t.Errorf("Categories() = %v, %v", cats, err)
	}
	tags, err := c.Tags()
	if err != nil || strings.Join(tags, ",") != "transfer,prowlarr" {
		t.Errorf("Tags() = %v, %v", tags, err)
	}

	hosts, err := c.TrackerHosts("abc")
	if err != nil || strings.Join(hosts, ",") != "tracker.example.org,backup.example.net" {
		t.Errorf("TrackerHosts() = %v, %v; want de-duplicated hosts with pseudo-trackers skipped", hosts, err)
	}
	if _, err := c.TrackerHosts("gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TrackerHosts(gone) err = %v, want ErrNotFound", err)
	}
}

func TestWrites(t *testing.T) {
	f := newFakeQbit(t)
	c := NewClient(f.URL, Auth{Username: "admin", Password: "secret"})

	if err := c.SetLocation([]string{"a", "b"}, "/seed/movies"); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateCategory("seed"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCategory([]string{"a"}, "seed"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAutoManagement([]string{"a"}, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete([]string{"a", "b"}, true); err != nil {
		t.Fatal(err)
	}
	var statusErr *StatusError
	if err := c.SetCategory([]string{"a"}, "missing"); !errors.As(err, &statusErr) || statusErr.Code != http.StatusConflict {
		t.Errorf("setCategory to a missing category: err = %v, want the 409 kept", err)
	}

	want := []postedForm{
		{"/api/v2/torrents/setLocation", map[string]string{"hashes": "a|b", "location": "/seed/movies"}},
		{"/api/v2/torrents/createCategory", map[string]string{"category": "seed", "savePath": ""}},
		{"/api/v2/torrents/setCategory", map[string]string{"hashes": "a", "category": "seed"}},
		{"/api/v2/torrents/setAutoManagement", map[string]string{"hashes": "a", "enable": "false"}},
		{"/api/v2/torrents/delete", map[string]string{"hashes": "a|b", "deleteFiles": "true"}},
		{"/api/v2/torrents/setCategory", map[string]string{"hashes": "a", "category": "missing"}},
	}
	got := f.posted()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("posted forms =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
}

func TestStates(t *testing.T) {
	for _, s := range []string{"moving", "checkingUP", "checkingDL", "checkingResumeData", "allocating"} {
		if !InFlight(s) {
			t.Errorf("InFlight(%q) = false", s)
		}
	}
	for _, s := range []string{"uploading", "stalledUP", "stoppedUP", "pausedUP", "downloading", "error", "somethingNew"} {
		if InFlight(s) {
			t.Errorf("InFlight(%q) = true", s)
		}
	}
	for _, s := range []string{"uploading", "stalledUP", "queuedUP", "forcedUP", "pausedUP", "stoppedUP"} {
		if !Seeding(s) {
			t.Errorf("Seeding(%q) = false", s)
		}
	}
	if Seeding("downloading") || Seeding("moving") || Seeding("somethingNew") {
		t.Error("only complete states count as seeding")
	}
	if !Errored("error") || !Errored("missingFiles") || Errored("stalledUP") {
		t.Error("Errored misclassifies")
	}
	if !Paused("stoppedDL") || !Paused("pausedUP") || Paused("stalledDL") {
		t.Error("Paused misclassifies")
	}
}

func TestTrackerHost(t *testing.T) {
	for raw, want := range map[string]string{
		"https://tracker.example.org/announce/abc": "tracker.example.org",
		"udp://OPEN.Tracker.net:6969":              "open.tracker.net",
		"** [LSD] **":                              "",
		"":                                         "",
		"no scheme here":                           "",
		"http://[::1]:8080/announce":               "::1",
	} {
		if got := TrackerHost(raw); got != want {
			t.Errorf("TrackerHost(%q) = %q, want %q", raw, got, want)
		}
	}
}
