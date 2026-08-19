package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// §8.1 — every piece of the cookie is pinned, because two readings of it
// are two implementations that cannot verify each other's cookies.
const (
	cookieName = "nag_session"

	// cookieMaxAge is the one constant both the issuer and the renewal
	// check read — hardcoding its derived numbers elsewhere breaks
	// silently the day Max-Age changes.
	cookieMaxAge = 365 * 24 * time.Hour

	// renewAfter: a valid cookie older than this is re-issued on the way
	// out, so a pinned tab never logs out.
	renewAfter = 30 * 24 * time.Hour
)

// cookieKey derives the signing key from NAG_TOKEN: the token is the key,
// the constant is the message. No second secret to manage, and rotating
// NAG_TOKEN invalidates every cookie — the log-out-everywhere lever (§8.1).
func cookieKey(token string) []byte {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("nag-cookie-v1"))
	return m.Sum(nil)
}

// signExpiry MACs the expiry as "v1|" + its decimal ASCII form, byte for
// byte — never big-endian bytes (§8.1).
func signExpiry(key []byte, exp string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("v1|" + exp))
	return m.Sum(nil)
}

// encodeSession builds the cookie value b64(exp) + "." + b64(mac), both
// halves unpadded base64url.
func encodeSession(key []byte, expiry time.Time) string {
	exp := strconv.FormatInt(expiry.Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(exp)) +
		"." + base64.RawURLEncoding.EncodeToString(signExpiry(key, exp))
}

// decodeSession returns the expiry of an authentic, unexpired cookie value.
func decodeSession(key []byte, value string, now time.Time) (expiry time.Time, ok bool) {
	expPart, macPart, found := strings.Cut(value, ".")
	if !found {
		return time.Time{}, false
	}
	exp, err := base64.RawURLEncoding.DecodeString(expPart)
	if err != nil {
		return time.Time{}, false
	}
	mac, err := base64.RawURLEncoding.DecodeString(macPart)
	if err != nil {
		return time.Time{}, false
	}
	if !hmac.Equal(mac, signExpiry(key, string(exp))) {
		return time.Time{}, false
	}
	unix, err := strconv.ParseInt(string(exp), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	expiry = time.Unix(unix, 0)
	if expiry.Before(now) {
		return time.Time{}, false
	}
	return expiry, true
}

// requireAuth guards /api/*: a bearer token or a session cookie (§8.1).
// A cookie inside the renewal window is re-issued on the way out.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.bearerOK(r) {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(cookieName); err == nil {
			now := time.Now()
			if expiry, ok := decodeSession(s.cookieKey, c.Value, now); ok {
				// "older than 30 days" computed from the expiry the cookie
				// carries — it holds nothing else.
				if expiry.Sub(now) < cookieMaxAge-renewAfter {
					s.setSessionCookie(w, now)
				}
				next.ServeHTTP(w, r)
				return
			}
		}
		writeError(w, http.StatusUnauthorized, "authentication required")
	})
}

// bearerOK accepts `Authorization: Bearer <NAG_TOKEN>` — the permanent
// path for curl, /api/push/test, and the future bookmarklet (§8.1).
func (s *Server) bearerOK(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	token, found := strings.CutPrefix(auth, "Bearer ")
	return found && subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) == 1
}

// handleLogin exchanges {token} for the session cookie. On failure it
// sleeps and serialises through a mutex — the entire rate limit, and
// sufficient for one user behind HTTPS (§8.1). The submitted token is
// never logged (§10.4).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "malformed json body")
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.token)) != 1 {
		s.loginMu.Lock()
		time.Sleep(s.loginSleep)
		s.loginMu.Unlock()
		writeError(w, http.StatusUnauthorized, "wrong token")
		return
	}
	s.setSessionCookie(w, time.Now())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setSessionCookie(w http.ResponseWriter, now time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    encodeSession(s.cookieKey, now.Add(cookieMaxAge)),
		Path:     "/",
		MaxAge:   int(cookieMaxAge / time.Second),
		HttpOnly: true,
		// Unconditional: behind Caddy the app sees plain HTTP, so deriving
		// this from r.TLS would silently ship a non-Secure cookie (§8.1).
		Secure: true,
		// Lax, not Strict — a notification click into a fresh tab must
		// arrive authenticated.
		SameSite: http.SameSiteLaxMode,
	})
}
