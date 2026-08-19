package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func stateWithETag(t *testing.T, h http.Handler, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStateETag(t *testing.T) {
	h := testServer(t, nil).Handler()
	createReminder(t, h, fmt.Sprintf(`{"text":"x","due_at":%d}`, time.Now().Unix()+3600))

	first := stateWithETag(t, h, "")
	etag := first.Header().Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("ETag = %q, want a weak validator", etag)
	}

	t.Run("unchanged data across two requests → 304, no body", func(t *testing.T) {
		rec := stateWithETag(t, h, etag)
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("304 carried a body: %q", rec.Body.String())
		}
	})

	t.Run("a changed row changes the tag", func(t *testing.T) {
		createReminder(t, h, fmt.Sprintf(`{"text":"y","due_at":%d}`, time.Now().Unix()+7200))
		rec := stateWithETag(t, h, etag)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 after a data change", rec.Code)
		}
		if rec.Header().Get("ETag") == etag {
			t.Error("ETag unchanged after a new row")
		}
	})
}

// The exclusion rule itself: two responses differing only in server_time
// share a tag (§8.2 — hashing it would mean the 304 never fires).
func TestStateETagExcludesServerTime(t *testing.T) {
	resp := stateResponse{
		ServerTime:   1000,
		OverdueCount: 1,
		Overdue:      []reminderResponse{{ID: 1, Text: "x", DueAt: 500, ExtraChannels: []string{}}},
		Later:        []reminderResponse{},
	}
	moved := resp
	moved.ServerTime = 2000
	if stateETag(resp) != stateETag(moved) {
		t.Error("server_time moved the tag")
	}

	changed := resp
	changed.Overdue = []reminderResponse{{ID: 2, Text: "x", DueAt: 500, ExtraChannels: []string{}}}
	if stateETag(resp) == stateETag(changed) {
		t.Error("a row change did not move the tag")
	}
}
