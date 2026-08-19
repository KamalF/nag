package httpapi

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/KamalF/nag/internal/config"
	"github.com/KamalF/nag/internal/store"
	_ "modernc.org/sqlite"
)

func testServer(t *testing.T, logs io.Writer) *Server {
	s, _ := testServerWithDB(t, logs)
	return s
}

func testServerWithDB(t *testing.T, logs io.Writer) (*Server, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "nag.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if logs == nil {
		logs = io.Discard
	}
	web := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<h1>test index</h1>")},
	}
	s := New(Options{
		Store:       st,
		Config:      testConfig(),
		Web:         web,
		Token:       testToken,
		VAPIDPublic: testVAPIDPublic,
		Log:         slog.New(slog.NewTextHandler(logs, nil)),
	})
	s.loginSleep = time.Millisecond // keep the §8.1 failure sleep out of test time
	return s, dbPath
}

const testVAPIDPublic = "test-vapid-public-key"

func ptr[T any](v T) *T { return &v }

func testConfig() *config.Config {
	return &config.Config{
		General: config.General{
			Timezone:      "Europe/Paris",
			DefaultPreset: "30min",
			RetentionDays: 30,
		},
		Presets: []config.Preset{
			{Key: "30min", Label: "30 min", Kind: "offset", Offset: ptr("30m"), Quick: true},
			{Key: "tomorrow", Label: "Tomorrow 9:00", Kind: "clock", At: ptr("09:00"), Days: ptr(1), Quick: true},
		},
	}
}

// insertChannel writes a channels row through a second connection on the
// same file — the CLI that owns channel writes is an M4 deliverable, and
// this mirrors its cross-process access (§4.3).
func insertChannel(t *testing.T, dbPath, name string, enabled bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(
		"INSERT INTO channels (name, url, enabled) VALUES (?, 'ntfy://host/topic', ?)",
		name, enabled); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// authed sends a request through h with the bearer token attached.
func authed(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRootServesIndexUnauthenticated(t *testing.T) {
	rec := get(t, testServer(t, nil).Handler(), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "test index") {
		t.Errorf("GET / body = %q, want index.html content", rec.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	s := testServer(t, nil)
	if rec := get(t, s.Handler(), "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", rec.Code)
	}
}

func TestHealthzOverBrokenDatabase(t *testing.T) {
	s := testServer(t, nil)
	s.store.Close()
	rec := get(t, s.Handler(), "/healthz")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /healthz = %d over a closed database, want 500", rec.Code)
	}
	assertErrorShape(t, rec, internalErrorMessage)
}

func TestAPICatchAll(t *testing.T) {
	h := testServer(t, nil).Handler()

	t.Run("misspelled endpoint is a JSON 404, not index.html", func(t *testing.T) {
		rec := authed(t, h, http.MethodGet, "/api/typo")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET /api/typo = %d, want 404", rec.Code)
		}
		assertErrorShape(t, rec, "")
	})

	t.Run("wrong method on a real path is 404, never 405", func(t *testing.T) {
		rec := authed(t, h, http.MethodPost, "/api/config")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("POST /api/config = %d, want the documented 404", rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "" {
			t.Errorf("Allow header = %q, want none (the catch-all answered)", allow)
		}
	})
}

// §10.4: mutations and errors get one line; successful GETs get none.
func TestRequestLogging(t *testing.T) {
	var logs strings.Builder
	h := testServer(t, &logs).Handler()

	get(t, h, "/healthz")
	if logs.Len() != 0 {
		t.Errorf("successful GET was logged: %q", logs.String())
	}

	authed(t, h, http.MethodGet, "/api/typo")
	line := logs.String()
	if !strings.Contains(line, "status=404") || !strings.Contains(line, "/api/typo") {
		t.Errorf("4xx line missing or incomplete: %q", line)
	}
}

func assertErrorShape(t *testing.T, rec *httptest.ResponseRecorder, wantMessage string) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not the {\"error\": …} shape: %v", rec.Body.String(), err)
	}
	msg, ok := body["error"]
	if !ok || len(body) != 1 {
		t.Fatalf("body = %v, want exactly one \"error\" field", body)
	}
	if wantMessage != "" && msg != wantMessage {
		t.Errorf("error = %q, want %q", msg, wantMessage)
	}
}
