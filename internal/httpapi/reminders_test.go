package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// authedJSON sends body through h with the bearer token.
func authedJSON(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func createReminder(t *testing.T, h http.Handler, body string) map[string]any {
	t.Helper()
	rec := authedJSON(t, h, http.MethodPost, "/api/reminders", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/reminders = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCreateReminderValidation(t *testing.T) {
	h := testServer(t, nil).Handler()
	future := time.Now().Unix() + 3600

	tests := []struct {
		name    string
		body    string
		wantErr []string
	}{
		{"missing text", fmt.Sprintf(`{"due_at":%d}`, future), []string{"text"}},
		{"empty after trimming", fmt.Sprintf(`{"text":"  \t ","due_at":%d}`, future), []string{"text"}},
		{"over 1000 bytes", fmt.Sprintf(`{"text":%q,"due_at":%d}`, strings.Repeat("x", 1001), future),
			[]string{"1001", "1000"}},
		{"newline names the character class", fmt.Sprintf(`{"text":"a\nb","due_at":%d}`, future),
			[]string{"control character"}},
		{"both preset and due_at", fmt.Sprintf(`{"text":"x","preset":"30min","due_at":%d}`, future),
			[]string{"not both"}},
		{"neither preset nor due_at", `{"text":"x"}`, []string{"preset", "due_at"}},
		{"unknown preset names the key", `{"text":"x","preset":"gone"}`, []string{`"gone"`}},
		{"due_at below range names both bounds", `{"text":"x","due_at":100}`,
			[]string{"946684800", "4102444800"}},
		{"millisecond timestamp rejected", fmt.Sprintf(`{"text":"x","due_at":%d}`, future*1000),
			[]string{"946684800", "4102444800"}},
		{"unknown channel named", fmt.Sprintf(`{"text":"x","due_at":%d,"extra_channels":["nope"]}`, future),
			[]string{`"nope"`}},
		{"unknown field rejected", fmt.Sprintf(`{"text":"x","due_at":%d,"quik":true}`, future),
			[]string{"malformed"}},
		{"malformed json", `{"text":`, []string{"malformed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := authedJSON(t, h, http.MethodPost, "/api/reminders", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d (%s), want 400", rec.Code, rec.Body.String())
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %s is not the error shape: %v", rec.Body.String(), err)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(body.Error, want) {
					t.Errorf("error %q does not contain %q", body.Error, want)
				}
			}
		})
	}

	t.Run("too many channels", func(t *testing.T) {
		names := make([]string, 17)
		for i := range names {
			names[i] = fmt.Sprintf("c%02d", i)
		}
		raw, _ := json.Marshal(names)
		rec := authedJSON(t, h, http.MethodPost, "/api/reminders",
			fmt.Sprintf(`{"text":"x","due_at":%d,"extra_channels":%s}`, future, raw))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "16") {
			t.Errorf("status = %d body = %s, want 400 naming the 16 cap", rec.Code, rec.Body.String())
		}
	})
}

func TestCreateReminderShape(t *testing.T) {
	h := testServer(t, nil).Handler()
	future := time.Now().Unix() + 3600
	got := createReminder(t, h, fmt.Sprintf(`{"text":"  buy milk  ","due_at":%d}`, future))

	if got["text"] != "buy milk" {
		t.Errorf("text = %q, want trimmed %q", got["text"], "buy milk")
	}
	if _, present := got["pushed_at"]; present {
		t.Error("pushed_at is in the reminder object — §8.2 forbids it")
	}
	if got["notified_at"] != nil || got["done_at"] != nil || got["delivery_error"] != nil {
		t.Errorf("future create carries stamps: %v", got)
	}
	if channels, ok := got["extra_channels"].([]any); !ok || len(channels) != 0 {
		t.Errorf("extra_channels = %v, want []", got["extra_channels"])
	}
}

func TestCreateWithPreset(t *testing.T) {
	h := testServer(t, nil).Handler()
	before := time.Now().Unix()
	got := createReminder(t, h, `{"text":"x","preset":"30min"}`)
	due := int64(got["due_at"].(float64))
	if due < before+29*60 || due > before+31*60 {
		t.Errorf("due_at = %d, want ~now+30m (now = %d)", due, before)
	}
}

// §4.1: a write that lands in the past never notifies — the same statement
// stamps both notified_at and pushed_at.
func TestBackdatedCreateCarriesBothStamps(t *testing.T) {
	s, _ := testServerWithDB(t, nil)
	h := s.Handler()
	past := time.Now().Unix() - 3600
	got := createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, past))

	if got["notified_at"] == nil {
		t.Error("backdated create: notified_at is null, want stamped")
	}
	id := int64(got["id"].(float64))
	rem, err := s.store.GetReminder(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rem.NotifiedAt == nil || rem.PushedAt == nil {
		t.Errorf("row stamps = notified %v pushed %v, want both set", rem.NotifiedAt, rem.PushedAt)
	}
}

func TestExtraChannelsCanonicalised(t *testing.T) {
	s, dbPath := testServerWithDB(t, nil)
	insertChannel(t, dbPath, "ntfy", true)
	insertChannel(t, dbPath, "telegram", false) // disabled is accepted at write time (§8.3)
	h := s.Handler()

	future := time.Now().Unix() + 3600
	got := createReminder(t, h, fmt.Sprintf(
		`{"text":"x","due_at":%d,"extra_channels":["telegram","ntfy","telegram"]}`, future))

	raw, _ := json.Marshal(got["extra_channels"])
	if string(raw) != `["ntfy","telegram"]` {
		t.Errorf("extra_channels = %s, want de-duplicated and sorted", raw)
	}
}

func TestState(t *testing.T) {
	h := testServer(t, nil).Handler()
	now := time.Now().Unix()
	createReminder(t, h, fmt.Sprintf(`{"text":"overdue","due_at":%d}`, now-3600))
	createReminder(t, h, fmt.Sprintf(`{"text":"later b","due_at":%d}`, now+7200))
	createReminder(t, h, fmt.Sprintf(`{"text":"later a","due_at":%d}`, now+3600))
	// same due_at as "later a": the id tiebreak keeps them stable (§8.2)
	createReminder(t, h, fmt.Sprintf(`{"text":"later a2","due_at":%d}`, now+3600))

	rec := authed(t, h, http.MethodGet, "/api/state")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/state = %d, want 200", rec.Code)
	}
	var state struct {
		ServerTime   int64            `json:"server_time"`
		OverdueCount int              `json:"overdue_count"`
		Overdue      []map[string]any `json:"overdue"`
		Later        []map[string]any `json:"later"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.ServerTime < now {
		t.Errorf("server_time = %d, want >= %d", state.ServerTime, now)
	}
	if state.OverdueCount != 1 || len(state.Overdue) != 1 {
		t.Errorf("overdue_count = %d, overdue = %d rows; want 1 and 1", state.OverdueCount, len(state.Overdue))
	}
	var laterTexts []string
	for _, r := range state.Later {
		laterTexts = append(laterTexts, r["text"].(string))
	}
	want := []string{"later a", "later a2", "later b"}
	if fmt.Sprint(laterTexts) != fmt.Sprint(want) {
		t.Errorf("later order = %v, want %v (due_at asc, id asc)", laterTexts, want)
	}
	if strings.Contains(rec.Body.String(), "pushed_at") {
		t.Error("pushed_at leaked into /api/state")
	}
}

// §10.4: the create's log line carries the new id from the response.
func TestCreateLogsTheNewID(t *testing.T) {
	var logs strings.Builder
	h := testServer(t, &logs).Handler()
	got := createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, time.Now().Unix()+3600))

	wantID := fmt.Sprintf("id=%.0f", got["id"].(float64))
	if !strings.Contains(logs.String(), wantID) {
		t.Errorf("log %q does not carry %s", logs.String(), wantID)
	}
	if strings.Contains(logs.String(), "buy milk") || strings.Contains(logs.String(), "text=") {
		t.Errorf("log carries reminder text: %q", logs.String())
	}
}
