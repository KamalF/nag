package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func patch(t *testing.T, h http.Handler, id any, body string) *httptest.ResponseRecorder {
	t.Helper()
	return authedJSON(t, h, http.MethodPatch, fmt.Sprintf("/api/reminders/%v", id), body)
}

func TestPatchValidation(t *testing.T) {
	h := testServer(t, nil).Handler()
	got := createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, time.Now().Unix()+3600))
	id := int64(got["id"].(float64))

	tests := []struct {
		name    string
		body    string
		wantErr []string
	}{
		{"empty object", `{}`, []string{"nothing to change"}},
		{"text null", `{"text":null}`, []string{"text", "cleared"}},
		{"text empty", `{"text":""}`, []string{"text"}},
		{"both timing fields", fmt.Sprintf(`{"preset":"30min","due_at":%d}`, time.Now().Unix()+60),
			[]string{"not both"}},
		{"unknown field names the lexically first", `{"zzz":1,"aaa":1,"text":"ok"}`,
			[]string{`"aaa"`}},
		{"due_at out of range", `{"due_at":12}`, []string{"946684800"}},
		{"unknown preset", `{"preset":"gone"}`, []string{`"gone"`}},
		{"new unknown channel", `{"extra_channels":["nope"]}`, []string{`"nope"`}},
		{"malformed json", `{"text":`, []string{"malformed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := patch(t, h, id, tt.body)
			assertBadRequest(t, rec, tt.wantErr...)
		})
	}

	t.Run("unknown id", func(t *testing.T) {
		if rec := patch(t, h, 9999, `{"text":"x"}`); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

func TestPatchReTimeUnclears(t *testing.T) {
	h := testServer(t, nil).Handler()
	got := createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, time.Now().Unix()-3600))
	id := int64(got["id"].(float64))
	authedJSON(t, h, http.MethodPost, fmt.Sprintf("/api/reminders/%d/done", id), "")

	future := time.Now().Unix() + 3600
	rec := patch(t, h, id, fmt.Sprintf(`{"due_at":%d}`, future))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var row map[string]any
	json.Unmarshal(rec.Body.Bytes(), &row)
	if row["done_at"] != nil {
		t.Error("re-time did not un-clear the row (§8.4)")
	}
	if row["notified_at"] != nil {
		t.Error("future re-time left notified_at set (§4.1)")
	}
	if int64(row["due_at"].(float64)) != future {
		t.Errorf("due_at = %v, want %d", row["due_at"], future)
	}
}

func TestPatchBackdatedReTimeStamps(t *testing.T) {
	s, _ := testServerWithDB(t, nil)
	h := s.Handler()
	got := createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, time.Now().Unix()+3600))
	id := int64(got["id"].(float64))

	rec := patch(t, h, id, fmt.Sprintf(`{"due_at":%d}`, time.Now().Unix()-60))
	var row map[string]any
	json.Unmarshal(rec.Body.Bytes(), &row)
	if row["notified_at"] == nil {
		t.Error("backdated re-time: notified_at null in response")
	}
	rem, err := s.store.GetReminder(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rem.PushedAt == nil {
		t.Error("backdated re-time: pushed_at not stamped (§4.1)")
	}
}

// §4.1: re-timing with an offset preset resolves from now, never from the
// old due_at.
func TestPatchOffsetPresetResolvesFromNow(t *testing.T) {
	h := testServer(t, nil).Handler()
	threeDaysAgo := time.Now().Unix() - 3*86400
	got := createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, threeDaysAgo))
	id := int64(got["id"].(float64))

	before := time.Now().Unix()
	rec := patch(t, h, id, `{"preset":"30min"}`)
	var row map[string]any
	json.Unmarshal(rec.Body.Bytes(), &row)
	due := int64(row["due_at"].(float64))
	if due < before+29*60 || due > before+31*60 {
		t.Errorf("due_at = %d, want ~now+30m — not old due_at+30m (%d)", due, threeDaysAgo+1800)
	}
}

func TestPatchChannelSemantics(t *testing.T) {
	s, dbPath := testServerWithDB(t, nil)
	insertChannel(t, dbPath, "gone", true)
	insertChannel(t, dbPath, "ntfy", true)
	h := s.Handler()

	future := time.Now().Unix() + 3600
	got := createReminder(t, h, fmt.Sprintf(
		`{"text":"x","due_at":%d,"extra_channels":["gone"]}`, future))
	id := int64(got["id"].(float64))
	deleteChannel(t, dbPath, "gone") // the row now carries an orphan

	t.Run("a stored orphan is carried forward", func(t *testing.T) {
		rec := patch(t, h, id, `{"text":"still fine","extra_channels":["gone"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("carrying the orphan = %d (%s), want 200", rec.Code, rec.Body.String())
		}
	})

	t.Run("a new unknown name is rejected even next to the orphan", func(t *testing.T) {
		rec := patch(t, h, id, `{"extra_channels":["gone","nope"]}`)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "nope") {
			t.Errorf("status = %d body = %s, want 400 naming \"nope\"", rec.Code, rec.Body.String())
		}
	})

	t.Run("null clears the list", func(t *testing.T) {
		rec := patch(t, h, id, `{"extra_channels":null}`)
		var row map[string]any
		json.Unmarshal(rec.Body.Bytes(), &row)
		if channels := row["extra_channels"].([]any); len(channels) != 0 {
			t.Errorf("extra_channels = %v, want cleared", channels)
		}
	})

	t.Run("the dropped orphan cannot be re-introduced", func(t *testing.T) {
		rec := patch(t, h, id, `{"extra_channels":["gone"]}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("re-introducing the orphan = %d, want 400 (it is no longer stored on the row)", rec.Code)
		}
	})
}

func deleteChannel(t *testing.T, dbPath, name string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM channels WHERE name = ?", name); err != nil {
		t.Fatal(err)
	}
}
