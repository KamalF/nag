package httpapi

import (
	"context"
	"net/http"
	"time"
)

type logAttrsKey struct{}

// setLogAttrs adds handler-specific fields to the request's §10.4 line.
// /api/push/test is the one handler that has neither a reminder id nor a
// single push endpoint to truncate. So it calls setLogAttrs to log what it
// does know instead.
func setLogAttrs(r *http.Request, attrs ...any) {
	if p, ok := r.Context().Value(logAttrsKey{}).(*[]any); ok {
		*p = append(*p, attrs...)
	}
}

// setLogID records the affected reminder id for the request's §10.4 log
// line. For the push endpoints, it records §7.5's truncated endpoint form
// instead. On a create, it is the only place that captures the new id. It
// sets the "id" field through setLogAttrs, and is not a second mechanism.
// Every caller calls it once, on the success path. So the result is the
// same as with a setter that overwrites, although setLogAttrs appends.
func setLogID(r *http.Request, id string) {
	setLogAttrs(r, "id", id)
}

// logRequests applies §10.4: one INFO line per mutating request, and one
// per 4xx/5xx for any method. It never logs a successful GET, so that the
// lines of the 45-second poll do not hide the lines that matter. A line
// never holds a token or reminder text.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		var extra []any
		r = r.WithContext(context.WithValue(r.Context(), logAttrsKey{}, &extra))
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
		attrs = append(attrs, extra...)
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
