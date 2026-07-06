// Package webui is Coldarr's optional web GUI: configure connections and
// tiers, see current disk usage, preview a move plan, and apply it. It's
// an alternative front end to the same config.Config / secrets.Store /
// engine.Engine the CLI uses - nothing here is a separate source of truth.
package webui

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/engine"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/mover"
	"github.com/vocoder/coldarr/internal/scheduler"
	"github.com/vocoder/coldarr/internal/secrets"
)

type Server struct {
	cfgPath   string
	connStore *secrets.Store
	pages     map[string]*template.Template

	mu  sync.RWMutex
	cfg *config.Config

	// applyMu guards currentRun - only one apply can be in flight at a
	// time, tracked here so the status page can be polled across
	// separate requests while it runs in the background.
	applyMu    sync.Mutex
	currentRun *applyRun

	// verifyMu guards currentVerify the same way applyMu guards
	// currentRun - only one size-verification run in flight at a time,
	// polled across requests while it runs in the background.
	verifyMu      sync.Mutex
	currentVerify *verifyProgress

	// schedMu guards the scheduler's in-memory (not persisted across
	// restarts) timing state. lastRunPlan/lastRunRescan are the due-check
	// anchor scheduler.Due compares against - reset both when a task
	// genuinely runs AND whenever its schedule is saved (so enabling or
	// editing a schedule can never itself trigger a surprise immediate
	// fire). lastRanPlan/lastRanRescan are the user-facing "last ran"
	// fact shown on the Scheduler settings page - unlike the anchor,
	// these are only ever updated by a genuine run, never by a save, so
	// the page never claims a task ran when it was really just edited.
	schedMu       sync.Mutex
	lastRunPlan   time.Time
	lastRunRescan time.Time
	lastRanPlan   time.Time
	lastRanRescan time.Time
	// rescanMu keeps a scheduled "Rescan Cold Storage" tick from
	// overlapping itself. It's read-only and independent of applyMu -
	// unlike a scheduled Plan run, it never competes with a manual Apply
	// click for the mover lock.
	rescanMu sync.Mutex

	authMu       sync.Mutex
	authSessions map[string]authSession
	oidcStates   map[string]oidcLoginState
}

type applyRun struct {
	progress *mover.Progress
	lock     *mover.Lock
}

func New(cfgPath string, cfg *config.Config, connStore *secrets.Store) (*Server, error) {
	pages, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{
		cfgPath:      cfgPath,
		cfg:          cfg,
		connStore:    connStore,
		pages:        pages,
		authSessions: map[string]authSession{},
		oidcStates:   map[string]oidcLoginState{},
	}, nil
}

type ListenOptions struct {
	Addr                     string
	TLSCertFile              string
	TLSKeyFile               string
	TrustedReverseProxyCIDRs string
}

func (o ListenOptions) Validate() error {
	if (o.TLSCertFile == "") != (o.TLSKeyFile == "") {
		return fmt.Errorf("both TLS certificate and key files are required when serving HTTPS")
	}
	if o.TrustedReverseProxyCIDRs != "" {
		if _, err := parseTrustedReverseProxyCIDRs(o.TrustedReverseProxyCIDRs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) ListenAndServe(opts ListenOptions) error {
	if err := opts.Validate(); err != nil {
		return err
	}

	handler := s.routes()
	if opts.TrustedReverseProxyCIDRs != "" {
		proxies, err := parseTrustedReverseProxyCIDRs(opts.TrustedReverseProxyCIDRs)
		if err != nil {
			return err
		}
		handler = trustedReverseProxyMiddleware(handler, proxies)
		log.Printf("coldarr web GUI trusting forwarded headers from %s", opts.TrustedReverseProxyCIDRs)
	}

	srv := &http.Server{Addr: opts.Addr, Handler: handler}
	if opts.TLSCertFile != "" {
		log.Printf("coldarr web GUI listening with HTTPS on %s", opts.Addr)
		return srv.ListenAndServeTLS(opts.TLSCertFile, opts.TLSKeyFile)
	}

	log.Printf("coldarr web GUI listening on %s", opts.Addr)
	return srv.ListenAndServe()
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /auth/login", s.handleOIDCLogin)
	mux.HandleFunc("GET /auth/callback", s.handleOIDCCallback)
	mux.HandleFunc("GET /auth/logout", s.handleLogout)

	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings/connections", http.StatusFound)
	})

	mux.HandleFunc("GET /settings/connections", s.handleConnectionsPage)
	mux.HandleFunc("POST /settings/connections/{app}/test", s.handleConnectionTest)
	mux.HandleFunc("POST /settings/connections/{app}", s.handleConnectionSave)
	mux.HandleFunc("POST /settings/connections/{app}/delete", s.handleConnectionDelete)

	mux.HandleFunc("GET /settings/tiers", s.handleTiersPage)
	mux.HandleFunc("GET /settings/tiers/new", s.handleTierNewForm)
	mux.HandleFunc("GET /settings/tiers/{name}/edit", s.handleTierEditForm)
	mux.HandleFunc("POST /settings/tiers", s.handleTierCreate)
	mux.HandleFunc("POST /settings/tiers/{name}", s.handleTierUpdate)
	mux.HandleFunc("POST /settings/tiers/{name}/delete", s.handleTierDelete)

	mux.HandleFunc("GET /settings/notifications", s.handleNotificationsPage)
	mux.HandleFunc("POST /settings/notifications", s.handleNotificationsSave)
	mux.HandleFunc("POST /settings/notifications/test", s.handleNotificationsTest)
	mux.HandleFunc("POST /settings/notifications/delete", s.handleNotificationsDelete)

	mux.HandleFunc("GET /settings/scheduler", s.handleSchedulerPage)
	mux.HandleFunc("POST /settings/scheduler/{task}", s.handleSchedulerSave)

	mux.HandleFunc("GET /settings/auth", s.handleAuthPage)
	mux.HandleFunc("POST /settings/auth", s.handleAuthSave)

	// Connections and Tiers moved under /settings - redirect anyone with
	// the old URLs bookmarked, same precedent as the /plan/apply/status
	// redirect below.
	mux.HandleFunc("GET /connections", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings/connections", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /tiers", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings/tiers", http.StatusMovedPermanently)
	})

	mux.HandleFunc("GET /plan", s.handlePlanPage)
	mux.HandleFunc("POST /plan/apply", s.handleApplyStart)
	mux.HandleFunc("GET /plan/apply/status/partial", s.handleApplyStatusPartial)
	// Apply status is now folded into /plan - redirect anyone with the old
	// URL bookmarked instead of just dropping it.
	mux.HandleFunc("GET /plan/apply/status", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/plan", http.StatusMovedPermanently)
	})

	mux.HandleFunc("GET /history", s.handleHistoryPage)
	mux.HandleFunc("POST /history/verify", s.handleVerifyStart)
	mux.HandleFunc("GET /history/verify/status", s.handleVerifyStatus)
	mux.HandleFunc("GET /history/verify/status/partial", s.handleVerifyStatusPartial)

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticSub)))

	return s.authMiddleware(mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// currentConfig returns a point-in-time copy of the live config, safe to
// read without holding the lock any longer than the copy itself.
func (s *Server) currentConfig() *config.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := *s.cfg
	return &cfg
}

func (s *Server) newEngine() (*engine.Engine, error) {
	return engine.New(s.currentConfig(), s.connStore)
}

// updateTiers applies fn to the current tier list, validates the result,
// persists it to coldarr.yaml, and swaps it into the live config, all
// under lock so concurrent requests can't interleave a read-modify-write.
func (s *Server) updateTiers(fn func([]model.Tier) ([]model.Tier, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	newTiers, err := fn(s.cfg.Tiers)
	if err != nil {
		return err
	}
	if err := config.ValidateTiers(newTiers, false); err != nil {
		return err
	}

	updated := *s.cfg
	updated.Tiers = newTiers
	if err := config.Save(s.cfgPath, &updated); err != nil {
		return err
	}
	s.cfg = &updated
	return nil
}

// updateNotifications persists the Verbose flag (the Apprise URL itself
// lives in connStore, not coldarr.yaml) and swaps it into the live
// config, under the same lock discipline as updateTiers.
func (s *Server) updateNotifications(verbose bool, tag string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	updated := *s.cfg
	updated.Notifications.Verbose = verbose
	updated.Notifications.Tag = tag
	if err := config.Save(s.cfgPath, &updated); err != nil {
		return err
	}
	s.cfg = &updated
	return nil
}

func (s *Server) updateAuthOIDC(auth config.OIDCAuthConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	updated := *s.cfg
	updated.Auth.OIDC = auth
	if err := config.Save(s.cfgPath, &updated); err != nil {
		return err
	}
	s.cfg = &updated
	return nil
}

// StartScheduler resets each currently-enabled task's due-check anchor to
// now - a process restart defers its next run rather than firing
// immediately, since none of the scheduler's timing state is persisted
// across restarts - then launches the background ticker that checks both
// tasks every tickInterval(). Meant to be called once, after New, before
// ListenAndServe; there's no corresponding Stop - like ListenAndServe
// itself, this runs for the life of the process.
func (s *Server) StartScheduler() {
	now := time.Now()
	cfg := s.currentConfig()
	if cfg.Scheduler.RunPlan.Enabled {
		s.touchPlanSchedule(now)
	}
	if cfg.Scheduler.RescanCold.Enabled {
		s.touchRescanSchedule(now)
	}

	go func() {
		ticker := time.NewTicker(tickInterval())
		defer ticker.Stop()
		for t := range ticker.C {
			s.tick(t)
		}
	}()
}

// tickInterval defaults to a minute, overridable via
// COLDARR_SCHEDULER_TICK_INTERVAL (a Go duration string) - the same
// env-override idiom Engine.Movers() uses for settle timing, and what
// makes the scheduler testable without waiting on real minutes/hours.
func tickInterval() time.Duration {
	if v := os.Getenv("COLDARR_SCHEDULER_TICK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return time.Minute
}

func (s *Server) tick(now time.Time) {
	cfg := s.currentConfig()

	if scheduler.Due(cfg.Scheduler.RunPlan, s.getLastRunPlan(), now) {
		s.runScheduledPlan(now)
	}
	if scheduler.Due(cfg.Scheduler.RescanCold, s.getLastRunRescan(), now) {
		s.runScheduledRescan(now)
	}
}

func (s *Server) getLastRunPlan() time.Time {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.lastRunPlan
}

func (s *Server) getLastRunRescan() time.Time {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.lastRunRescan
}

func (s *Server) getLastRanPlan() time.Time {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.lastRanPlan
}

func (s *Server) getLastRanRescan() time.Time {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.lastRanRescan
}

// touchPlanSchedule resets run_plan's due-check anchor without recording
// a genuine run - called when the schedule itself is saved (see
// updateSchedule) or the process starts, so enabling or editing it can
// never trigger a surprise immediate fire.
func (s *Server) touchPlanSchedule(t time.Time) {
	s.schedMu.Lock()
	s.lastRunPlan = t
	s.schedMu.Unlock()
}

func (s *Server) touchRescanSchedule(t time.Time) {
	s.schedMu.Lock()
	s.lastRunRescan = t
	s.schedMu.Unlock()
}

// recordPlanRan records that run_plan genuinely executed at t - resets
// the due-check anchor (so it isn't considered due again until the next
// full period) and updates the "last ran" fact shown on the Scheduler
// settings page.
func (s *Server) recordPlanRan(t time.Time) {
	s.schedMu.Lock()
	s.lastRunPlan = t
	s.lastRanPlan = t
	s.schedMu.Unlock()
}

func (s *Server) recordRescanRan(t time.Time) {
	s.schedMu.Lock()
	s.lastRunRescan = t
	s.lastRanRescan = t
	s.schedMu.Unlock()
}

// updateSchedule validates and persists a single named task's schedule
// ("run_plan" or "rescan_cold"), then always resets that task's due-check
// anchor to now - whether enabling, disabling, or just adjusting the time
// - so saving a schedule can never itself trigger an immediate unattended
// run as a surprise side effect. This does not touch the "last ran" fact
// shown on the settings page - only a genuine run does that.
func (s *Server) updateSchedule(task string, sched scheduler.Schedule) error {
	if err := scheduler.Validate(sched); err != nil {
		return err
	}

	s.mu.Lock()
	updated := *s.cfg
	switch task {
	case "run_plan":
		updated.Scheduler.RunPlan = sched
	case "rescan_cold":
		updated.Scheduler.RescanCold = sched
	}
	if err := config.Save(s.cfgPath, &updated); err != nil {
		s.mu.Unlock()
		return err
	}
	s.cfg = &updated
	s.mu.Unlock()

	now := time.Now()
	switch task {
	case "run_plan":
		s.touchPlanSchedule(now)
	case "rescan_cold":
		s.touchRescanSchedule(now)
	}
	return nil
}

// render executes the template into a buffer first, not directly into w -
// html/template writes incrementally as it executes, so writing straight
// to the ResponseWriter means a template error partway through has
// already sent a 200 and some bytes, and the subsequent http.Error ends
// up trying to send a second status header ("superfluous
// response.WriteHeader call"), producing a garbled response. Buffering
// means a failed render never reaches the client at all - just a clean
// 500.
func (s *Server) render(w http.ResponseWriter, page string, data any) {
	s.renderTemplate(w, page, "layout", data)
}

func (s *Server) renderPartial(w http.ResponseWriter, page, partial string, data any) {
	s.renderTemplate(w, page, partial, data)
}

func (s *Server) renderTemplate(w http.ResponseWriter, page, name string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
