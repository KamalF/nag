package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/KamalF/nag/internal/store"
)

// reminderID parses the {id} path value. A value that cannot be a stored
// id, a non-numeric one included, gets the same §8.3 unknown-{id} 404 as an
// id that is not stored. The caller writes that 404.
func reminderID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// handleDone is POST /api/reminders/{id}/done → 200 + the reminder.
func (s *Server) handleDone(w http.ResponseWriter, r *http.Request) {
	s.lifecycleUpdate(w, r, s.store.MarkDone)
}

// handleUndone is POST /api/reminders/{id}/undone → 200 + the reminder.
// The §4.1 pushed_at guard is in the store. notified_at in the response
// tells a caller which of the two undone cases applied.
func (s *Server) handleUndone(w http.ResponseWriter, r *http.Request) {
	s.lifecycleUpdate(w, r, s.store.MarkUndone)
}

// lifecycleUpdate runs one store operation that takes the id and the current
// time. It answers 200 with the row as it stands, also when the operation
// was a no-op.
func (s *Server) lifecycleUpdate(w http.ResponseWriter, r *http.Request,
	update func(ctx context.Context, id, now int64) (store.Reminder, error)) {
	id, ok := reminderID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "no such reminder")
		return
	}
	rem, err := update(r.Context(), id, time.Now().Unix())
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no such reminder")
		return
	}
	if err != nil {
		s.log.Error("reminder update", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	setLogID(r, strconv.FormatInt(id, 10))
	writeJSON(w, http.StatusOK, toReminderResponse(rem))
}

// handleDelete is DELETE /api/reminders/{id} → 204, hard delete.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := reminderID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "no such reminder")
		return
	}
	existed, err := s.store.DeleteReminder(r.Context(), id)
	if err != nil {
		s.log.Error("delete reminder", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	if !existed {
		writeError(w, http.StatusNotFound, "no such reminder")
		return
	}
	setLogID(r, strconv.FormatInt(id, 10))
	w.WriteHeader(http.StatusNoContent)
}
