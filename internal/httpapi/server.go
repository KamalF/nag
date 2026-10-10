// Package httpapi is the HTTP surface (§8): handlers, middleware, and the
// §8.3 error shape.
package httpapi

import (
	"context"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/KamalF/nag/internal/config"
	"github.com/KamalF/nag/internal/notify"
	"github.com/KamalF/nag/internal/store"
)

// pushSender is the §7.1 send-and-handle helper as §7.5 needs it: one
// send, one verdict, the response ladder already applied. The sweep holds
// the same helper (§7.3) — one copy of that ladder, two callers, and the
// deadline is the only thing that differs between them.
type pushSender interface {
	SendTo(ctx context.Context, perSend time.Duration, payload []byte, sub store.Subscription) notify.SendResult
}

// Go's table has no entry for .webmanifest, so the file server falls back to
// sniffing and answers text/plain — which browsers accept today and which
// X-Content-Type-Options: nosniff (§10.2) makes a real risk tomorrow. The
// manifest is the reason iPhone push works at all (§9.3), so it is not a file
// to leave on a guess.
func init() {
	mime.AddExtensionType(".webmanifest", "application/manifest+json")
}

// Server timeouts (§8.2) — the defaults are unlimited, and this process is
// the one that must still be alive in a year. WriteTimeout is also the
// number §7.5's push-test send budget is derived from.
const (
	ReadHeaderTimeout = 10 * time.Second
	ReadTimeout       = 15 * time.Second
	WriteTimeout      = 15 * time.Second
	IdleTimeout       = 120 * time.Second
)

// maxBodyBytes caps every request body (§8.2): `text` is limited to 1000
// bytes, but nothing else would stop a client streaming a gigabyte into
// the decoder first.
const maxBodyBytes = 16 << 10

type Server struct {
	store *store.Store
	live  *config.Live // load once per request: one snapshot throughout (§5.5)
	web   fs.FS
	etags map[string]string // path → content hash of each embedded asset (§10.2)
	log   *slog.Logger

	token     string
	cookieKey []byte

	push       pushSender
	pushBudget time.Duration

	// loginSlot serialises failed logins: one token held at a time
	loginSlot  chan struct{}
	loginSleep time.Duration
}

type Options struct {
	Store  *store.Store
	Config *config.Live
	Web    fs.FS
	Token  string
	Push   pushSender
	Log    *slog.Logger
}

func New(o Options) *Server {
	return &Server{
		store:      o.Store,
		live:       o.Config,
		web:        o.Web,
		etags:      assetETags(o.Web),
		log:        o.Log,
		token:      o.Token,
		cookieKey:  cookieKey(o.Token),
		push:       o.Push,
		pushBudget: pushTestBudget,
		loginSlot:  make(chan struct{}, 1),
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
	// wins). FileServer does not check the method, so a POST to a static path
	// is answered like a GET — harmless, since nothing static mutates.
	mux.Handle("/", http.FileServerFS(s.web))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/config", s.handleConfig)
	api.HandleFunc("GET /api/channels", s.handleChannels)
	api.HandleFunc("GET /api/state", s.handleState)
	api.HandleFunc("POST /api/reminders", s.handleCreateReminder)
	api.HandleFunc("POST /api/reminders/{id}/done", s.handleDone)
	api.HandleFunc("POST /api/reminders/{id}/undone", s.handleUndone)
	api.HandleFunc("PATCH /api/reminders/{id}", s.handlePatch)
	api.HandleFunc("DELETE /api/reminders/{id}", s.handleDelete)
	api.HandleFunc("POST /api/push/subscribe", s.handleSubscribe)
	api.HandleFunc("POST /api/push/unsubscribe", s.handleUnsubscribe)
	api.HandleFunc("POST /api/push/test", s.handlePushTest)
	api.HandleFunc("/api/", s.handleAPINotFound)
	mux.Handle("/api/", s.requireAuth(api))

	return s.logRequests(securityHeaders(s.caching(capBody(requireJSONBody(mux)))))
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
