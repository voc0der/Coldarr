// Package webui is Coldarr's optional web GUI: configure connections and
// tiers, see current disk usage, preview a move plan, and apply it. It's
// an alternative front end to the same config.Config / secrets.Store /
// engine.Engine the CLI uses - nothing here is a separate source of truth.
package webui

import (
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"sync"

	"github.com/vocoder/coldarr/internal/config"
	"github.com/vocoder/coldarr/internal/engine"
	"github.com/vocoder/coldarr/internal/model"
	"github.com/vocoder/coldarr/internal/secrets"
)

type Server struct {
	cfgPath   string
	connStore *secrets.Store
	pages     map[string]*template.Template

	mu  sync.RWMutex
	cfg *config.Config
}

func New(cfgPath string, cfg *config.Config, connStore *secrets.Store) (*Server, error) {
	pages, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	return &Server{cfgPath: cfgPath, cfg: cfg, connStore: connStore, pages: pages}, nil
}

func (s *Server) ListenAndServe(addr string) error {
	log.Printf("coldarr web GUI listening on %s", addr)
	return http.ListenAndServe(addr, s.routes())
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	mux.HandleFunc("GET /connections", s.handleConnectionsPage)
	mux.HandleFunc("POST /connections/{app}/test", s.handleConnectionTest)
	mux.HandleFunc("POST /connections/{app}", s.handleConnectionSave)
	mux.HandleFunc("POST /connections/{app}/delete", s.handleConnectionDelete)

	mux.HandleFunc("GET /tiers", s.handleTiersPage)
	mux.HandleFunc("GET /tiers/new", s.handleTierNewForm)
	mux.HandleFunc("GET /tiers/{name}/edit", s.handleTierEditForm)
	mux.HandleFunc("POST /tiers", s.handleTierCreate)
	mux.HandleFunc("POST /tiers/{name}", s.handleTierUpdate)
	mux.HandleFunc("POST /tiers/{name}/delete", s.handleTierDelete)

	mux.HandleFunc("GET /plan", s.handlePlanPage)
	mux.HandleFunc("POST /plan/apply", s.handleApply)

	mux.HandleFunc("GET /history", s.handleHistoryPage)

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticSub)))

	return mux
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

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderPartial(w http.ResponseWriter, page, partial string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, partial, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
