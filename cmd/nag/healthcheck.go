package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// healthcheckTimeout is §10.1's bound: Docker's HEALTHCHECK allows 5 s.
const healthcheckTimeout = 2 * time.Second

// runHealthcheck is `nag healthcheck` for the Dockerfile's HEALTHCHECK. The
// scratch image has no curl, so the binary asks its own /healthz. /healthz
// answers 200 only after SELECT 1, and the command exits 0 only on a 200. It
// needs NAG_ADDR and nothing else. Nothing acts on the result (§10.1): it is
// a status to read in `docker ps`, not a self-heal. The check catches a
// database that became unopenable. A restart loop against a bad disk or a
// wrong chown replaces one visible unhealthy status with log lines that
// nobody reads.
func runHealthcheck(stderr io.Writer) int {
	url, err := healthcheckURL(envOr("NAG_ADDR", defaultAddr))
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return 1
	}
	client := &http.Client{Timeout: healthcheckTimeout}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "unhealthy: %s answered %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}

// healthcheckURL derives the URL from NAG_ADDR. It keeps the port. If the
// host is empty, 0.0.0.0 or ::, it dials 127.0.0.1, because a
// listen-on-everything address is not an address to dial. Otherwise it uses
// the host as given. This rule needs no second env var, and stays correct if
// NAG_ADDR is later narrowed to one interface.
func healthcheckURL(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("NAG_ADDR %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
