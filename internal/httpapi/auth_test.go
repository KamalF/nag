package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "test-token-of-sufficient-length"

// The §8.1 encoding, rebuilt independently: exp as a decimal ASCII string,
// k = HMAC(NAG_TOKEN, "nag-cookie-v1"), mac = HMAC(k, "v1|"+exp), value
// b64(exp)+"."+b64(mac) in unpadded base64url. Two readings of §8.1 are
// two implementations that cannot verify each other's cookies — this test
// is the second reading.
func TestCookieEncodingIsPinned(t *testing.T) {
	expiry := time.Unix(1786742400, 0)
	exp := "1786742400"

	keyMAC := hmac.New(sha256.New, []byte(testToken))
	keyMAC.Write([]byte("nag-cookie-v1"))
	key := keyMAC.Sum(nil)

	macMAC := hmac.New(sha256.New, key)
	macMAC.Write([]byte("v1|" + exp))
	want := base64.RawURLEncoding.EncodeToString([]byte(exp)) +
		"." + base64.RawURLEncoding.EncodeToString(macMAC.Sum(nil))

	if got := encodeSession(cookieKey(testToken), expiry); got != want {
		t.Errorf("encodeSession = %q, want %q", got, want)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	key := cookieKey(testToken)
	now := time.Unix(1755600000, 0)
	expiry := now.Add(cookieMaxAge)

	got, ok := decodeSession(key, encodeSession(key, expiry), now)
	if !ok {
		t.Fatal("authentic cookie rejected")
	}
	if !got.Equal(time.Unix(expiry.Unix(), 0)) {
		t.Errorf("expiry = %v, want %v", got, expiry)
	}
}

func TestSessionRejections(t *testing.T) {
	key := cookieKey(testToken)
	now := time.Unix(1755600000, 0)
	valid := encodeSession(key, now.Add(cookieMaxAge))
	expPart, macPart, _ := strings.Cut(valid, ".")

	otherExp := base64.RawURLEncoding.EncodeToString([]byte("1999999999"))
	tests := map[string]string{
		"tampered expiry":            otherExp + "." + macPart,
		"tampered mac":               expPart + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"no separator":               expPart + macPart,
		"padded base64":              expPart + "=." + macPart,
		"empty value":                "",
		"non-decimal expiry":         base64.RawURLEncoding.EncodeToString([]byte("tomorrow")) + "." + macPart,
		"cookie under another token": encodeSession(cookieKey("some-other-token-just-as-long"), now.Add(cookieMaxAge)),
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			if _, ok := decodeSession(key, value, now); ok {
				t.Errorf("decodeSession accepted %q", value)
			}
		})
	}

	t.Run("expired cookie", func(t *testing.T) {
		old := encodeSession(key, now.Add(-time.Second))
		if _, ok := decodeSession(key, old, now); ok {
			t.Error("decodeSession accepted an expired cookie")
		}
	})
}

func TestLoginIssuesThePinnedCookie(t *testing.T) {
	h := testServer(t, nil).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(`{"token":"`+testToken+`"}`)))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /login = %d, want 204", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName {
		t.Fatalf("cookies = %v, want one %s", cookies, cookieName)
	}
	c := cookies[0]
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("attributes = HttpOnly:%v Secure:%v SameSite:%v Path:%q, want the §8.1 set",
			c.HttpOnly, c.Secure, c.SameSite, c.Path)
	}
	if want := int(cookieMaxAge / time.Second); c.MaxAge != want {
		t.Errorf("Max-Age = %d, want %d", c.MaxAge, want)
	}
	if _, ok := decodeSession(cookieKey(testToken), c.Value, time.Now()); !ok {
		t.Error("issued cookie does not verify")
	}
}

func TestLoginWrongToken(t *testing.T) {
	h := testServer(t, nil).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(`{"token":"wrong"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /login = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a failed login set a cookie")
	}
	assertErrorShape(t, rec, "wrong token")
}

func TestLogoutClearsTheCookie(t *testing.T) {
	h := testServer(t, nil).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/logout", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /logout = %d, want 204", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != -1 || cookies[0].Value != "" {
		t.Errorf("cookies = %v, want one cleared %s", cookies, cookieName)
	}
}

func TestAPIRequiresAuth(t *testing.T) {
	h := testServer(t, nil).Handler()

	t.Run("nothing → 401 in the error shape", func(t *testing.T) {
		rec := get(t, h, "/api/typo")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		assertErrorShape(t, rec, "")
	})

	t.Run("bearer accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/typo", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound { // through auth, into the catch-all
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("wrong bearer rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/typo", nil)
		req.Header.Set("Authorization", "Bearer nope")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("valid cookie accepted", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/typo", nil)
		req.AddCookie(&http.Cookie{Name: cookieName,
			Value: encodeSession(cookieKey(testToken), time.Now().Add(cookieMaxAge))})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

// The renewal threshold is computed from cookieMaxAge, never hardcoded:
// a cookie younger than 30 days passes untouched, an older one is
// re-issued on the way out (§8.1).
func TestSlidingRenewal(t *testing.T) {
	h := testServer(t, nil).Handler()
	key := cookieKey(testToken)

	request := func(expiry time.Time) []*http.Cookie {
		req := httptest.NewRequest(http.MethodGet, "/api/typo", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: encodeSession(key, expiry)})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (authenticated)", rec.Code)
		}
		return rec.Result().Cookies()
	}

	t.Run("fresh cookie is not re-issued", func(t *testing.T) {
		if cookies := request(time.Now().Add(cookieMaxAge - time.Hour)); len(cookies) != 0 {
			t.Errorf("a fresh cookie was re-issued: %v", cookies)
		}
	})

	t.Run("cookie past the renewal age is re-issued", func(t *testing.T) {
		aged := time.Now().Add(cookieMaxAge - renewAfter - time.Hour)
		cookies := request(aged)
		if len(cookies) != 1 {
			t.Fatalf("aged cookie not re-issued (cookies = %v)", cookies)
		}
		expiry, ok := decodeSession(key, cookies[0].Value, time.Now())
		if !ok {
			t.Fatal("re-issued cookie does not verify")
		}
		if expiry.Before(time.Now().Add(cookieMaxAge - time.Minute)) {
			t.Errorf("re-issued expiry %v is not a full Max-Age ahead", expiry)
		}
	})
}
