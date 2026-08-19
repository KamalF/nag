package httpapi

import (
	"context"
	"net/http"
	"time"
)

type logIDKey struct{}

// setLogID records the affected reminder id — or §7.5's truncated endpoint
// form for the push endpoints — for the request's §10.4 log line. On a
// create it is the only place the new id is captured.
func setLogID(r *http.Request, id string) {
	if p, ok := r.Context().Value(logIDKey{}).(*string); ok {
		*p = id
	}
}

// logRequests applies §10.4: one INFO line per mutating request and one
// per 4xx/5xx whatever the method — successful GETs are never logged, or
// the 45-second poll would bury everything that matters. Never a token,
// never reminder text.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var id string
		r = r.WithContext(context.WithValue(r.Context(), logIDKey{}, &id))
		next.ServeHTTP(rec, r)

		mutating := r.Method != http.MethodGet && r.Method != http.MethodHead
		if !mutating && rec.status < 400 {
			return
		}
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond).String(),
		}
		if id != "" {
			attrs = append(attrs, "id", id)
		}
		s.log.Info("request", attrs...)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
