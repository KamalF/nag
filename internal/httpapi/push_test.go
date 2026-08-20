package httpapi

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func b64url(n int, fill byte) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

const testEndpoint = "https://push.example.net/send/AbCdEfGh12345678"

func subscribeBody(endpoint, p256dh, auth, serverKey string) string {
	raw, _ := json.Marshal(map[string]any{
		"endpoint":               endpoint,
		"keys":                   map[string]string{"p256dh": p256dh, "auth": auth},
		"application_server_key": serverKey,
	})
	return string(raw)
}

type subscriptionRow struct {
	P256dh, Auth, VAPIDPublic string
	CreatedAt                 int64
}

func readSubscription(t *testing.T, dbPath, endpoint string) (subscriptionRow, int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM push_subscriptions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	var row subscriptionRow
	err = db.QueryRow(
		"SELECT p256dh, auth, vapid_public, created_at FROM push_subscriptions WHERE endpoint = ?",
		endpoint).Scan(&row.P256dh, &row.Auth, &row.VAPIDPublic, &row.CreatedAt)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return row, count
}

func TestSubscribeUpsert(t *testing.T) {
	s, dbPath := testServerWithDB(t, nil)
	h := s.Handler()

	first := authedJSON(t, h, http.MethodPost, "/api/push/subscribe",
		subscribeBody(testEndpoint, b64url(65, 1), b64url(16, 1), b64url(65, 9)))
	if first.Code != http.StatusNoContent || first.Body.Len() != 0 {
		t.Fatalf("subscribe = %d body %q, want bare 204", first.Code, first.Body.String())
	}
	original, count := readSubscription(t, dbPath, testEndpoint)
	if count != 1 {
		t.Fatalf("rows = %d, want 1", count)
	}
	if original.VAPIDPublic != b64url(65, 9) {
		t.Errorf("vapid_public = %q, want the posted application_server_key — never the server's own", original.VAPIDPublic)
	}

	// re-post under a rotated key: same endpoint, new p256dh/auth/key
	second := authedJSON(t, h, http.MethodPost, "/api/push/subscribe",
		subscribeBody(testEndpoint, b64url(65, 2), b64url(16, 2), b64url(65, 8)))
	if second.Code != http.StatusNoContent {
		t.Fatalf("second subscribe = %d, want 204", second.Code)
	}
	updated, count := readSubscription(t, dbPath, testEndpoint)
	if count != 1 {
		t.Fatalf("rows after upsert = %d, want 1", count)
	}
	if updated.P256dh != b64url(65, 2) || updated.Auth != b64url(16, 2) || updated.VAPIDPublic != b64url(65, 8) {
		t.Errorf("upsert did not overwrite all three key columns: %+v", updated)
	}
	if updated.CreatedAt != original.CreatedAt {
		t.Errorf("created_at moved from %d to %d, want preserved", original.CreatedAt, updated.CreatedAt)
	}
}

func TestSubscribeShapeChecks(t *testing.T) {
	h := testServer(t, nil).Handler()
	valid := func() (string, string, string, string) {
		return testEndpoint, b64url(65, 1), b64url(16, 1), b64url(65, 9)
	}
	tests := []struct {
		name    string
		body    func() string
		wantErr string
	}{
		{"http endpoint", func() string {
			_, p, a, k := valid()
			return subscribeBody("http://push.example.net/x", p, a, k)
		}, "endpoint"},
		{"relative endpoint", func() string {
			_, p, a, k := valid()
			return subscribeBody("/send/abc", p, a, k)
		}, "endpoint"},
		{"padded application_server_key", func() string {
			e, p, a, _ := valid()
			return subscribeBody(e, p, a, base64.URLEncoding.EncodeToString(make([]byte, 65)))
		}, "application_server_key"},
		{"short p256dh", func() string {
			e, _, a, k := valid()
			return subscribeBody(e, b64url(64, 1), a, k)
		}, "keys.p256dh"},
		{"wrong-length auth", func() string {
			e, p, _, k := valid()
			return subscribeBody(e, p, b64url(15, 1), k)
		}, "keys.auth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := authedJSON(t, h, http.MethodPost, "/api/push/subscribe", tt.body())
			assertBadRequest(t, rec, tt.wantErr)
		})
	}
}

func TestUnsubscribeAndPushSubscribed(t *testing.T) {
	s, _ := testServerWithDB(t, nil)
	h := s.Handler()

	pushSubscribed := func() bool {
		var state struct {
			PushSubscribed bool `json:"push_subscribed"`
		}
		json.Unmarshal(authed(t, h, http.MethodGet, "/api/state").Body.Bytes(), &state)
		return state.PushSubscribed
	}

	if pushSubscribed() {
		t.Fatal("push_subscribed true with no rows")
	}
	authedJSON(t, h, http.MethodPost, "/api/push/subscribe",
		subscribeBody(testEndpoint, b64url(65, 1), b64url(16, 1), b64url(65, 9)))
	if !pushSubscribed() {
		t.Fatal("push_subscribed false after subscribe")
	}

	rec := authedJSON(t, h, http.MethodPost, "/api/push/unsubscribe",
		fmt.Sprintf(`{"endpoint":%q}`, testEndpoint))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unsubscribe = %d, want 204", rec.Code)
	}
	if pushSubscribed() {
		t.Error("push_subscribed true after unsubscribe")
	}

	if rec := authedJSON(t, h, http.MethodPost, "/api/push/unsubscribe",
		`{"endpoint":"https://push.example.net/gone"}`); rec.Code != http.StatusNoContent {
		t.Errorf("unsubscribing a stranger = %d, want 204", rec.Code)
	}
}

// §10.4: the push endpoints log the truncated form in the id field, never
// the URL they were handed.
func TestPushEndpointsLogTruncatedForm(t *testing.T) {
	var logs strings.Builder
	h := testServer(t, &logs).Handler()
	authedJSON(t, h, http.MethodPost, "/api/push/subscribe",
		subscribeBody(testEndpoint, b64url(65, 1), b64url(16, 1), b64url(65, 9)))

	line := logs.String()
	if strings.Contains(line, "/send/AbCdEfGh12345678") {
		t.Errorf("full endpoint leaked into the log: %q", line)
	}
	if !strings.Contains(line, "12345678") || !strings.Contains(line, "push.example.net") {
		t.Errorf("truncated form missing from the log: %q", line)
	}
}
