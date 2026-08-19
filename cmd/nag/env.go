package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// §5.1 defaults. Only `serve` enforces the full table; each other
// subcommand reads just the values it needs — genkeys and version run
// with nothing set.
const (
	defaultDB     = "/data/nag.db"
	defaultConfig = "/config/nag.toml"
	defaultAddr   = ":8080"
)

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// serveEnv is the fully validated §5.1 table.
type serveEnv struct {
	db, config, addr string
	token            string
	vapidPublic      string
	vapidPrivate     string
	vapidSubject     string
}

const genkeysFix = "  Generate one:  docker compose run --rm -T nag genkeys >> nag.env\n  Then restart."

// loadServeEnv resolves and shape-checks every §5.1 variable. Each refusal
// names the variable, the expected shape, and what was found — a mangled
// VAPID paste must be a startup error here, not a push failure on every
// sweep once a reminder comes due.
func loadServeEnv() (serveEnv, error) {
	e := serveEnv{
		db:           envOr("NAG_DB", defaultDB),
		config:       envOr("NAG_CONFIG", defaultConfig),
		addr:         envOr("NAG_ADDR", defaultAddr),
		token:        os.Getenv("NAG_TOKEN"),
		vapidPublic:  os.Getenv("NAG_VAPID_PUBLIC"),
		vapidPrivate: os.Getenv("NAG_VAPID_PRIVATE"),
		vapidSubject: os.Getenv("NAG_VAPID_SUBJECT"),
	}
	if e.vapidPublic == "" && e.vapidPrivate == "" {
		return e, fmt.Errorf("no VAPID keypair configured.\n%s", genkeysFix)
	}
	if err := checkToken(e.token); err != nil {
		return e, err
	}
	if err := checkVAPIDKey("NAG_VAPID_PUBLIC", e.vapidPublic, 65); err != nil {
		return e, err
	}
	if err := checkVAPIDKey("NAG_VAPID_PRIVATE", e.vapidPrivate, 32); err != nil {
		return e, err
	}
	return e, checkSubject(e.vapidSubject)
}

// checkToken enforces the 20-character floor: NAG_TOKEN is not only the
// bearer credential but the sole input to the cookie signing key (§8.1),
// and a six-character token is a six-character MAC key that fails silently.
func checkToken(token string) error {
	if token == "" {
		return fmt.Errorf("NAG_TOKEN is not set (expected the bearer token, minimum 20 characters).\n%s", genkeysFix)
	}
	if len(token) < 20 {
		return fmt.Errorf("NAG_TOKEN is %d characters, minimum is 20 — it is also the cookie signing key, and a short token is a short key", len(token))
	}
	return nil
}

// checkVAPIDKey applies the same shape check §8.2 applies to a
// client-supplied application_server_key: unpadded base64url decoding to
// exactly wantBytes.
func checkVAPIDKey(name, value string, wantBytes int) error {
	if value == "" {
		return fmt.Errorf("%s is not set (expected unpadded base64url decoding to %d bytes).\n%s",
			name, wantBytes, genkeysFix)
	}
	// Go's base64 decoder ignores \r and \n, and a trailing \r from a
	// TTY-corrupted `genkeys` append (§5.2) is the very paste this check
	// exists to catch — reject it before decoding.
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s contains a carriage return or newline — expected one line of unpadded base64url (%d decoded bytes)",
			name, wantBytes)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return fmt.Errorf("%s is not unpadded base64url (expected %d decoded bytes): found %q",
			name, wantBytes, value)
	}
	if len(decoded) != wantBytes {
		return fmt.Errorf("%s decodes to %d bytes, expected exactly %d", name, len(decoded), wantBytes)
	}
	return nil
}

// checkSubject wants an absolute mailto: or https: URL (RFC 8292) and
// rejects the genkeys placeholder by name: push services use the subject
// to reach the operator, and a placeholder that boots is one that ships.
func checkSubject(subject string) error {
	if subject == "" {
		return fmt.Errorf("NAG_VAPID_SUBJECT is not set (expected an absolute mailto: or https: URL)")
	}
	if subject == placeholderSubject {
		return fmt.Errorf("NAG_VAPID_SUBJECT is still the placeholder %s that genkeys emits — put your own address there",
			placeholderSubject)
	}
	u, err := url.Parse(subject)
	if err != nil || !u.IsAbs() || (u.Scheme != "mailto" && u.Scheme != "https") {
		return fmt.Errorf("NAG_VAPID_SUBJECT %q must be an absolute mailto: or https: URL", subject)
	}
	return nil
}
