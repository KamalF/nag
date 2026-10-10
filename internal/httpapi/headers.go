package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"strings"
)

// contentSecurityPolicy is §10.2's pinned policy, byte for byte. Nothing is
// loaded from another origin, so a strict policy costs nothing and closes
// the injected-script class. data: on img-src is the canvas favicon;
// font-src 'none' is deliberate — fonts are system stacks, which CSP never
// sees, so no font request is legitimate. style-src 'self' also forbids a
// style attribute in the markup (§9.7), while leaving CSSOM alone.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; font-src 'none'; connect-src 'self'; " +
	"object-src 'none'; base-uri 'none'; form-action 'self'; " +
	"frame-ancestors 'none'"

// securityHeaders sets §10.2's three headers on every response, from one
// middleware, before anything else can answer.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// assetETags hashes every embedded file once, at startup, keyed by the
// path it is served at — "/" for index.html, which the file server answers
// there.
func assetETags(web fs.FS) map[string]string {
	tags := map[string]string{}
	fs.WalkDir(web, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(web, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		path := "/" + name
		if name == "index.html" {
			path = "/"
		}
		tags[path] = `"` + hex.EncodeToString(sum[:8]) + `"`
		return nil
	})
	return tags
}

// caching is §10.2's split. The embedded assets get no-cache plus an ETag
// derived from their content — one cheap revalidation per load, and a
// deploy takes effect at once; a max-age is how a tab pinned for a year
// ends up running last year's code. The file server answers If-None-Match
// itself once the ETag header is set. Everything else, /api/* included, is
// no-store.
func (s *Server) caching(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := s.etags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// sameOriginOnly is §10.2's first CSRF rule: a browser request that is not
// same-origin is refused on every mutating method, by the standard
// library's check of Sec-Fetch-Site (or Origin against Host). SameSite=Lax
// alone is not enough: it is about the site, so a page on a sibling
// subdomain still gets the cookie sent with an empty form POST to /done.
// A request with neither header is curl or another non-browser client,
// which authenticates with the bearer token and passes.
func sameOriginOnly(next http.Handler) http.Handler {
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "cross-origin request refused")
	}))
	return protection.Handler(next)
}

// requireJSONBody is §10.2's second CSRF rule: a mutating request that
// carries a body must say application/json, which an HTML form cannot
// produce. Bodyless mutations — /done, /undone, DELETE, the push test —
// are exempt, so their curl invocations stay bare; sameOriginOnly is what
// covers them in a browser. /logout is exempt outright: the worst a
// forged form gets there is logging you out of your own browser.
func requireJSONBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mutating(r.Method) && r.ContentLength != 0 && r.URL.Path != "/logout" {
			mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || !strings.EqualFold(mediaType, "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "a request with a body must be sent as application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
