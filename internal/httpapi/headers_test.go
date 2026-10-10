package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// §10.2: the three headers on every response class — a static asset, an
// API success, an API error, the catch-all, the health check, a failed
// login — with the CSP compared byte for byte.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	const csp = "default-src 'self'; script-src 'self'; style-src 'self'; " +
		"img-src 'self' data:; font-src 'none'; connect-src 'self'; " +
		"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	h := testServer(t, nil).Handler()
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/", nil),
		httptest.NewRequest(http.MethodGet, "/app.js", nil),
		authedRequest(http.MethodGet, "/api/state"),
		authedRequest(http.MethodGet, "/api/typo"),
		httptest.NewRequest(http.MethodGet, "/api/state", nil), // 401
		httptest.NewRequest(http.MethodGet, "/healthz", nil),
		loginRequest(t.Context(), "wrong"),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		hdr := rec.Header()
		name := req.Method + " " + req.URL.Path
		if got := hdr.Get("Content-Security-Policy"); got != csp {
			t.Errorf("%s: CSP = %q", name, got)
		}
		if hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: nosniff/referrer missing: %v", name, hdr)
		}
	}
}

func authedRequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	return req
}

// The caching split: /api/* is no-store; assets are no-cache with an ETag
// from their content, and a matching If-None-Match answers 304.
func TestCachingSplit(t *testing.T) {
	h := testServer(t, nil).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authedRequest(http.MethodGet, "/api/state"))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("/api/state Cache-Control = %q, want no-store", got)
	}

	for _, path := range []string{"/", "/app.js", "/sw.js", "/manifest.webmanifest"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		etag := rec.Header().Get("ETag")
		if rec.Header().Get("Cache-Control") != "no-cache" || etag == "" {
			t.Errorf("%s: Cache-Control %q, ETag %q", path, rec.Header().Get("Cache-Control"), etag)
			continue
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("If-None-Match", etag)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("%s with a matching If-None-Match = %d, want 304", path, rec.Code)
		}
	}
}

// §10.2's CSRF rule: a bodied mutation needs application/json; a bodyless
// one — /done, the push test — does not, and neither does /logout.
func TestJSONContentTypeRule(t *testing.T) {
	h := testServer(t, nil).Handler()
	created := createReminder(t, h, `{"text":"x","preset":"30min"}`)
	id := int64(created["id"].(float64))

	bodied := func(contentType string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/reminders",
			strings.NewReader(`{"text":"y","preset":"30min"}`))
		req.Header.Set("Authorization", "Bearer "+testToken)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		if code := bodied(ct); code != http.StatusUnsupportedMediaType {
			t.Errorf("bodied POST with Content-Type %q = %d, want 415", ct, code)
		}
	}
	if code := bodied("application/json; charset=utf-8"); code != http.StatusCreated {
		t.Errorf("bodied POST as application/json = %d, want 201", code)
	}

	for _, req := range []*http.Request{
		authedRequest(http.MethodPost, fmt.Sprintf("/api/reminders/%d/done", id)),
		authedRequest(http.MethodPost, "/api/push/test"),
		httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader("x=1")),
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnsupportedMediaType {
			t.Errorf("%s %s was refused for its Content-Type", req.Method, req.URL.Path)
		}
	}
}
