package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestDoneUndoneFlow(t *testing.T) {
	h := testServer(t, nil).Handler()
	got := createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, time.Now().Unix()+3600))
	id := int64(got["id"].(float64))

	done := authedJSON(t, h, http.MethodPost, fmt.Sprintf("/api/reminders/%d/done", id), "")
	if done.Code != http.StatusOK {
		t.Fatalf("done = %d, want 200", done.Code)
	}
	var row map[string]any
	json.Unmarshal(done.Body.Bytes(), &row)
	if row["done_at"] == nil {
		t.Error("done_at is null after /done")
	}

	// the cleared row leaves both state lists
	var state struct {
		Overdue, Later []map[string]any
	}
	rec := authed(t, h, http.MethodGet, "/api/state")
	json.Unmarshal(rec.Body.Bytes(), &state)
	if len(state.Overdue)+len(state.Later) != 0 {
		t.Errorf("cleared row still listed: %v / %v", state.Overdue, state.Later)
	}

	// double-tap: still 200, still done
	again := authedJSON(t, h, http.MethodPost, fmt.Sprintf("/api/reminders/%d/done", id), "")
	if again.Code != http.StatusOK {
		t.Errorf("second done = %d, want 200 (no-op)", again.Code)
	}

	undone := authedJSON(t, h, http.MethodPost, fmt.Sprintf("/api/reminders/%d/undone", id), "")
	if undone.Code != http.StatusOK {
		t.Fatalf("undone = %d, want 200", undone.Code)
	}
	json.Unmarshal(undone.Body.Bytes(), &row)
	if row["done_at"] != nil {
		t.Error("done_at still set after /undone")
	}
}

func TestDeleteReminderEndpoint(t *testing.T) {
	h := testServer(t, nil).Handler()
	got := createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, time.Now().Unix()+3600))
	id := int64(got["id"].(float64))

	rec := authedJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/reminders/%d", id), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("delete carried a body: %q", rec.Body.String())
	}
	if rec := authedJSON(t, h, http.MethodDelete, fmt.Sprintf("/api/reminders/%d", id), ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", rec.Code)
	}
}

func TestLifecycleUnknownIDsAre404(t *testing.T) {
	h := testServer(t, nil).Handler()
	for _, path := range []string{
		"/api/reminders/9999/done",
		"/api/reminders/9999/undone",
		"/api/reminders/abc/done",
	} {
		rec := authedJSON(t, h, http.MethodPost, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, rec.Code)
		}
		assertErrorShape(t, rec, "no such reminder")
	}
	if rec := authedJSON(t, h, http.MethodDelete, "/api/reminders/9999", ""); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown = %d, want 404", rec.Code)
	}
}
