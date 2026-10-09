package webui

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/scheduler"
	"github.com/vocoder/coldarr/internal/secrets"
)

func TestListenOptions_Validate(t *testing.T) {
	tests := []struct {
		name    string
		opts    ListenOptions
		wantErr string
	}{
		{name: "plain HTTP", opts: ListenOptions{Addr: ":8478"}},
		{name: "HTTPS", opts: ListenOptions{Addr: ":8478", TLSCertFile: "cert.pem", TLSKeyFile: "key.pem"}},
		{name: "trusted proxies", opts: ListenOptions{Addr: ":8478", TrustedReverseProxyCIDRs: "10.0.0.0/8, 172.16.0.0/12"}},
		{name: "certificate without a key", opts: ListenOptions{TLSCertFile: "cert.pem"}, wantErr: "both TLS certificate and key files are required"},
		{name: "key without a certificate", opts: ListenOptions{TLSKeyFile: "key.pem"}, wantErr: "both TLS certificate and key files are required"},
		{name: "bad proxy CIDR", opts: ListenOptions{TrustedReverseProxyCIDRs: "10.0.0.0/33"}, wantErr: "parsing trusted reverse proxy CIDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.Validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestListenAndServe_ReportsWhyItCannotStart: ListenAndServe only ever
// returns when the server can't run, and then it has to say why - a bad
// option, a port that's taken, or a TLS key pair it can't load.
func TestListenAndServe_ReportsWhyItCannotStart(t *testing.T) {
	srv := newSettingsTestServer(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	dir := t.TempDir()

	tests := []struct {
		name    string
		opts    ListenOptions
		wantErr string
	}{
		{name: "invalid options", opts: ListenOptions{Addr: "127.0.0.1:0", TLSKeyFile: "key.pem"}, wantErr: "both TLS certificate and key files"},
		{name: "port taken", opts: ListenOptions{Addr: taken.Addr().String()}, wantErr: "address already in use"},
		{name: "port taken behind a trusted proxy", opts: ListenOptions{Addr: taken.Addr().String(), TrustedReverseProxyCIDRs: "10.0.0.0/8"}, wantErr: "address already in use"},
		{
			name:    "TLS key pair missing",
			opts:    ListenOptions{Addr: "127.0.0.1:0", TLSCertFile: filepath.Join(dir, "cert.pem"), TLSKeyFile: filepath.Join(dir, "key.pem")},
			wantErr: "cert.pem",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := srv.ListenAndServe(tt.opts); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ListenAndServe = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestStartScheduler_NeverRunsATaskOnBoot pins the restart invariant from
// the scheduler's own entry point: a task that would already be due when
// Coldarr starts is armed for its next slot, not run as Coldarr comes up.
func TestStartScheduler_NeverRunsATaskOnBoot(t *testing.T) {
	t.Setenv("COLDARR_SCHEDULER_TICK_INTERVAL", "10ms")
	dir, hotDir, coldDir := testTierDirs(t)
	radarr := newFakeRadarr(t, hotDir)
	srv := newTestServer(t, dir, hotDir, coldDir, radarr.URL, "", false)
	srv.cfg.Scheduler.ScanCutoffs = scheduler.Schedule{Enabled: true, Unit: scheduler.Hourly, Every: 1} // never run, so due now

	started := time.Now()
	srv.StartScheduler()
	time.Sleep(60 * time.Millisecond) // several ticks

	if hits := radarr.cutoffHits(); hits != 0 {
		t.Fatalf("quality-cutoff scans = %d, want none on boot", hits)
	}
	if !srv.getLastRanScanCutoffs().IsZero() {
		t.Error("arming at startup must not be recorded as a run")
	}
	if anchor := srv.getLastRunScanCutoffs(); anchor.Before(started) {
		t.Errorf("anchor = %v, want the task armed at startup (%v or later)", anchor, started)
	}
}

func TestTickInterval(t *testing.T) {
	for value, want := range map[string]time.Duration{"": time.Minute, "15s": 15 * time.Second, "soon": time.Minute} {
		t.Setenv("COLDARR_SCHEDULER_TICK_INTERVAL", value)
		if got := tickInterval(); got != want {
			t.Errorf("COLDARR_SCHEDULER_TICK_INTERVAL=%q: tickInterval() = %v, want %v", value, got, want)
		}
	}
}

// TestRoutes_OldURLsStillLand: bookmarks from before Connections, Tiers and
// the apply status page moved still reach the page that replaced them.
func TestRoutes_OldURLsStillLand(t *testing.T) {
	t.Setenv(passwordEnvVar, "pw")
	handler := newAuthTestServer(t, false).routes()
	cookie := passwordSession(t, handler)

	for target, want := range map[string]struct {
		code     int
		location string
	}{
		"/settings":          {http.StatusFound, "/settings/connections"},
		"/connections":       {http.StatusMovedPermanently, "/settings/connections"},
		"/tiers":             {http.StatusMovedPermanently, "/settings/tiers"},
		"/plan/apply/status": {http.StatusMovedPermanently, "/plan"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.AddCookie(cookie)
		handler.ServeHTTP(rec, req)
		if rec.Code != want.code || rec.Header().Get("Location") != want.location {
			t.Errorf("GET %s = %d %q, want %d %q", target, rec.Code, rec.Header().Get("Location"), want.code, want.location)
		}
	}
}

// TestRender_FailedTemplateIsACleanServerError: pages render into a buffer
// first, so a template that fails partway sends a plain 500 - never half a
// page with a second status line tacked on.
func TestRender_FailedTemplateIsACleanServerError(t *testing.T) {
	srv := newSettingsTestServer(t)
	rec := httptest.NewRecorder()
	srv.render(rec, "dashboard", 42) // the dashboard can't render from an int
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "<html") || strings.Contains(body, "<nav") {
		t.Fatalf("a failed render leaked a partial page:\n%s", body)
	}
}

// TestNew_RefusesUnreadableState: each state file kept beside coldarr.yaml
// failing to parse stops startup with an error naming it, rather than
// starting with that state silently wiped.
func TestNew_RefusesUnreadableState(t *testing.T) {
	for file, wantErr := range map[string]string{
		"coldarr-linkcache.json": "parsing link cache",
		"coldarr-orphans.json":   "parsing orphan scan cache",
		"coldarr-scheduler.json": "parsing scheduler state",
	} {
		t.Run(file, func(t *testing.T) {
			t.Setenv(passwordEnvVar, "pw")
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, file), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := secrets.LoadOrCreate(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := New(filepath.Join(dir, "coldarr.yaml"), &config.Config{}, store); err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("New = %v, want an error containing %q", err, wantErr)
			}
		})
	}

	t.Run("password file missing", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(passwordFileEnvVar, filepath.Join(dir, "no-such-secret"))
		store, err := secrets.LoadOrCreate(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := New(filepath.Join(dir, "coldarr.yaml"), &config.Config{}, store); err == nil || !strings.Contains(err.Error(), passwordFileEnvVar) {
			t.Fatalf("New = %v, want an error naming %s", err, passwordFileEnvVar)
		}
	})
}

func TestLoadSchedulerState(t *testing.T) {
	dir := t.TempDir()

	if state, err := loadSchedulerState(filepath.Join(dir, "missing.json")); err != nil || len(state) != 0 {
		t.Errorf("missing file = (%v, %v), want an empty state", state, err)
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if state, err := loadSchedulerState(empty); err != nil || len(state) != 0 {
		t.Errorf("empty file = (%v, %v), want an empty state", state, err)
	}

	if _, err := loadSchedulerState(dir); err == nil || !strings.Contains(err.Error(), "reading scheduler state") {
		t.Errorf("a directory = %v, want a read error", err)
	}
}

// TestSchedulerState_FailedWriteKeepsTheRunInMemory: a task that genuinely
// ran stays recorded for this process even when its timestamp can't be
// persisted - a write failure is logged, never turned into a failed run.
func TestSchedulerState_FailedWriteKeepsTheRunInMemory(t *testing.T) {
	srv := newSettingsTestServer(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	srv.schedStatePath = filepath.Join(blocker, "coldarr-scheduler.json")

	now := time.Now()
	srv.recordTaskRan(taskRescanCold, now)
	if got := srv.getLastRanRescan(); !got.Equal(now) {
		t.Fatalf("last ran = %v, want %v kept in memory", got, now)
	}
	if _, err := os.Stat(srv.schedStatePath); err == nil {
		t.Fatal("expected nothing written under a regular file")
	}

	for _, breakWrite := range []func(path string) error{
		func(path string) error { return os.Mkdir(path+".tmp", 0o750) }, // temp file can't be written
		func(path string) error { return os.Mkdir(path, 0o750) },        // can't be renamed into place
	} {
		path := filepath.Join(t.TempDir(), "coldarr-scheduler.json")
		if err := breakWrite(path); err != nil {
			t.Fatal(err)
		}
		if err := writeSchedulerState(path, map[string]taskRunState{taskRunPlan: {LastRan: now}}); err == nil {
			t.Errorf("writeSchedulerState(%s) = nil, want the failed write reported", path)
		}
	}
}

// TestSchedulerState_WorksWithoutPersistence: a Server built without a
// state file (as several tests build one) still tracks runs in memory.
func TestSchedulerState_WorksWithoutPersistence(t *testing.T) {
	now := time.Now()

	s := &Server{cfg: &config.Config{}}
	s.recordTaskRan(taskRunPlan, now)
	if !s.getLastRanPlan().Equal(now) || !s.getLastRunPlan().Equal(now) {
		t.Errorf("recordTaskRan without persistence: ran %v, anchor %v, want both %v", s.getLastRanPlan(), s.getLastRunPlan(), now)
	}

	s = &Server{cfg: &config.Config{}}
	s.armTask(taskScanOrphans, scheduler.Schedule{Enabled: true, Unit: scheduler.Hourly, Every: 1}, now)
	if !s.getLastRunScanOrphans().Equal(now) {
		t.Errorf("armTask without persistence: anchor %v, want %v", s.getLastRunScanOrphans(), now)
	}

	if got := scheduleFor(&config.Config{}, "defrag"); got != (scheduler.Schedule{}) {
		t.Errorf("scheduleFor(unknown task) = %+v, want the zero schedule", got)
	}
}

// TestScheduleSaves_FailedWriteLeavesTheScheduleAlone: a schedule that
// can't be written to coldarr.yaml isn't applied either, so what runs never
// differs from what a restart would load.
func TestScheduleSaves_FailedWriteLeavesTheScheduleAlone(t *testing.T) {
	srv := newSettingsTestServer(t)
	if err := os.Mkdir(srv.cfgPath+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}

	body := serveForm(srv.handleSchedulerSave, "/settings/scheduler/rescan_cold",
		url.Values{"enabled": {"on"}, "unit": {"hourly"}, "every": {"1"}}, "task", "rescan_cold").Body.String()
	if !strings.Contains(body, "alert-error") || strings.Contains(body, "Schedule saved.") {
		t.Errorf("expected the failed save reported:\n%s", body)
	}
	if srv.currentConfig().Scheduler.RescanCold.Enabled {
		t.Error("a schedule that couldn't be saved must not be applied")
	}
	if !srv.getLastRunRescan().IsZero() {
		t.Error("a schedule that couldn't be saved must not be armed")
	}

	body = serveForm(srv.handleSchedulerSave, "/settings/scheduler/omit-days", url.Values{"omit_days": {"saturday"}}, "task", "omit-days").Body.String()
	if strings.Contains(body, "Weekly omit days saved.") || len(srv.currentConfig().Scheduler.WeeklyOmitDays) != 0 {
		t.Errorf("omit days that couldn't be saved must not be applied:\n%s", body)
	}
	if err := srv.updateWeeklyOmitDays([]scheduler.Weekday{"funday"}); err == nil {
		t.Error("updateWeeklyOmitDays must validate what it's given")
	}
}

func TestItemLinks_PreferTheExternalURL(t *testing.T) {
	srv := newSettingsTestServer(t)
	if err := srv.connStore.Set("radarr", secrets.Connection{URL: "http://radarr:7878", APIKey: "k", ExternalURL: "https://radarr.example.com/"}); err != nil {
		t.Fatal(err)
	}
	src := srv.buildLinkSources()

	links := itemLinks(src, "radarr", "movie a", "")
	if len(links) != 1 || links[0].URL != "https://radarr.example.com/movie/movie%20a" {
		t.Fatalf("links = %+v, want the external URL with the slug escaped", links)
	}
	if links := itemLinks(src, "sonarr", "show-a", ""); len(links) != 0 {
		t.Errorf("links = %+v, want none for an app that isn't configured", links)
	}
	if links := itemLinks(src, "radarr", "", "/cold/Movie A"); len(links) != 0 {
		t.Errorf("links = %+v, want none without a slug or a Jellyfin match", links)
	}
}
