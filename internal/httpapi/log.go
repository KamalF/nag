package httpapi

import (
	"net/http"
	"time"
)

// logRequests applies §10.4: one INFO line per mutating request and one
// per 4xx/5xx whatever the method — successful GETs are never logged, or
// the 45-second poll would bury everything that matters. Never a token,
// never reminder text.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		mutating := r.Method != http.MethodGet && r.Method != http.MethodHead
		if !mutating && rec.status < 400 {
			return
		}
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond).String(),
		)
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
