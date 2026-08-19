// Package httpapi is the HTTP surface (§8): handlers, middleware, and the
// §8.3 error shape.
package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/KamalF/nag/internal/config"
	"github.com/KamalF/nag/internal/store"
)

// Server timeouts (§8.2) — the defaults are unlimited, and this process is
// the one that must still be alive in a year. WriteTimeout is also the
// number §7.5's push-test send budget is derived from.
const (
	ReadHeaderTimeout = 10 // seconds
	ReadTimeout       = 15
	WriteTimeout      = 15
	IdleTimeout       = 120
)

// maxBodyBytes caps every request body (§8.2): `text` is limited to 1000
// bytes, but nothing else would stop a client streaming a gigabyte into
// the decoder first.
const maxBodyBytes = 16 << 10

type Server struct {
	store *store.Store
	cfg   *config.Config
	loc   *time.Location // general.timezone, resolved once — the config is validated
	web   fs.FS
	log   *slog.Logger

	token     string
	cookieKey []byte

	loginMu    sync.Mutex
	loginSleep time.Duration
}

func New(st *store.Store, cfg *config.Config, web fs.FS, token string, log *slog.Logger) *Server {
	loc, err := time.LoadLocation(cfg.General.Timezone)
	if err != nil {
		loc = time.UTC // unreachable behind config validation (§5.5)
	}
	return &Server{
		store:      st,
		cfg:        cfg,
		loc:        loc,
		web:        web,
		log:        log,
		token:      token,
		cookieKey:  cookieKey(token),
		loginSleep: 250 * time.Millisecond,
	}
}

// Handler assembles the mux. `GET /` (the embedded frontend) is
// unauthenticated (§8.1); `/api/` is registered explicitly as a catch-all
// 404 so a misspelled endpoint fails in the §8.3 shape instead of falling
// through to index.html under a 200 — which also, deliberately, answers a
// wrong method on a real path with 404, never 405 (§8.2).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Methodless on purpose: a method-specific "GET /" plus the methodless
	// "/api/" catch-all is a ServeMux precedence conflict (neither pattern
	// wins), and FileServer already answers non-GET methods with 405.
	mux.Handle("/", http.FileServerFS(s.web))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/state", s.handleState)
	api.HandleFunc("POST /api/reminders", s.handleCreateReminder)
	api.HandleFunc("POST /api/reminders/{id}/done", s.handleDone)
	api.HandleFunc("POST /api/reminders/{id}/undone", s.handleUndone)
	api.HandleFunc("DELETE /api/reminders/{id}", s.handleDelete)
	api.HandleFunc("/api/", s.handleAPINotFound)
	mux.Handle("/api/", s.requireAuth(api))

	return s.logRequests(capBody(mux))
}

// handleHealthz answers 200 only after the database answered SELECT 1 —
// otherwise it would return 200 over a broken database (§8.2). No auth.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Healthy(r.Context()); err != nil {
		s.log.Error("healthz: database check failed", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "no such api endpoint")
}

func capBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}
