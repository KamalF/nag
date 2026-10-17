package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// §10.1's host rules: keep the port; a listen-on-everything host dials
// 127.0.0.1; an explicit host is used as given.
func TestHealthcheckURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080/healthz",
		"0.0.0.0:8080":   "http://127.0.0.1:8080/healthz",
		"[::]:8080":      "http://127.0.0.1:8080/healthz",
		"127.0.0.1:9000": "http://127.0.0.1:9000/healthz",
		"nag.lan:8080":   "http://nag.lan:8080/healthz",
		"[::1]:8080":     "http://[::1]:8080/healthz",
	} {
		got, err := healthcheckURL(addr)
		if err != nil || got != want {
			t.Errorf("healthcheckURL(%q) = %q, %v; want %q", addr, got, err, want)
		}
	}
	if _, err := healthcheckURL("8080"); err == nil {
		t.Error("a NAG_ADDR without a port was accepted")
	}
}

// Exit 0 only on a 200.
func TestHealthcheckExitCode(t *testing.T) {
	for status, want := range map[int]int{http.StatusOK: 0, http.StatusInternalServerError: 1} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				t.Errorf("dialed %s", r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		setVars(t, map[string]string{"NAG_ADDR": strings.TrimPrefix(srv.URL, "http://")})
		var stdout, stderr strings.Builder
		if got := run([]string{"healthcheck"}, &stdout, &stderr); got != want {
			t.Errorf("healthcheck against %d = %d, want %d (%s)", status, got, want, stderr.String())
		}
		srv.Close()
	}
	// nothing listening
	setVars(t, map[string]string{"NAG_ADDR": "127.0.0.1:1"})
	var stdout, stderr strings.Builder
	if got := run([]string{"healthcheck"}, &stdout, &stderr); got != 1 {
		t.Errorf("healthcheck with nothing listening = %d, want 1", got)
	}
}
