package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/KamalF/nag/internal/store"
)

// handlePatch is PATCH /api/reminders/{id} — the only write path for both
// a correction and a re-snooze (§8.4). Presence detection needs a map
// decode (§8.3): absent means unchanged, null and empty are answers of
// their own, and a struct decode cannot tell those apart.
func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	id, ok := reminderID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "no such reminder")
		return
	}
	current, err := s.store.GetReminder(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no such reminder")
		return
	}
	if err != nil {
		s.log.Error("patch: read reminder", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}

	var fields map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		writeError(w, http.StatusBadRequest, "malformed json body")
		return
	}
	if len(fields) == 0 {
		writeError(w, http.StatusBadRequest, "nothing to change")
		return
	}

	upd, errMessage, err := s.buildUpdate(r.Context(), fields, current)
	if err != nil {
		s.log.Error("patch: build update", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	if errMessage != "" {
		writeError(w, http.StatusBadRequest, errMessage)
		return
	}

	rem, err := s.store.UpdateReminder(r.Context(), current, upd, time.Now().Unix())
	if errors.Is(err, sql.ErrNoRows) {
		// deleted between our read and the update — §8.3's unknown-{id} 404,
		// not a 500
		writeError(w, http.StatusNotFound, "no such reminder")
		return
	}
	if err != nil {
		s.log.Error("patch: update reminder", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	setLogID(r, strconv.FormatInt(id, 10))
	writeJSON(w, http.StatusOK, toReminderResponse(rem))
}

// buildUpdate consumes the known keys from fields and rejects what is
// left, naming the lexically first leftover — Go randomises map
// iteration, so "the first one" is chosen, not taken (§8.3).
func (s *Server) buildUpdate(ctx context.Context, fields map[string]json.RawMessage, current store.Reminder) (store.ReminderUpdate, string, error) {
	var upd store.ReminderUpdate

	if raw, present := consume(fields, "text"); present {
		var text *string
		if err := json.Unmarshal(raw, &text); err != nil || text == nil {
			return upd, "text cannot be cleared", nil
		}
		trimmed, errMessage := validateText(*text)
		if errMessage != "" {
			return upd, errMessage, nil
		}
		upd.Text = &trimmed
	}

	rawPreset, hasPreset := consume(fields, "preset")
	rawDueAt, hasDueAt := consume(fields, "due_at")
	if hasPreset && hasDueAt {
		return upd, "send either preset or due_at, not both", nil
	}
	if hasPreset || hasDueAt {
		var preset *string
		var dueAt *int64
		if hasPreset {
			if err := json.Unmarshal(rawPreset, &preset); err != nil || preset == nil {
				return upd, "preset must be a string", nil
			}
		} else {
			if err := json.Unmarshal(rawDueAt, &dueAt); err != nil || dueAt == nil {
				return upd, "due_at must be an integer", nil
			}
		}
		// re-time: an offset preset resolves from now, never from the old
		// due_at (§4.1)
		due, errMessage := s.resolveDueAt(preset, dueAt, time.Now())
		if errMessage != "" {
			return upd, errMessage, nil
		}
		upd.DueAt = &due
	}

	if raw, present := consume(fields, "extra_channels"); present {
		var names []string
		if string(raw) != "null" {
			if err := json.Unmarshal(raw, &names); err != nil {
				return upd, "extra_channels must be an array of names or null", nil
			}
		}
		// a name already stored on the row is carried, never re-checked —
		// the orphan asymmetry (§8.3)
		canonical, errMessage, err := s.canonicalChannels(ctx, names, current.ExtraChannels)
		if err != nil || errMessage != "" {
			return upd, errMessage, err
		}
		if canonical == nil {
			canonical = []string{}
		}
		upd.ExtraChannels = &canonical
	}

	if len(fields) > 0 {
		leftover := make([]string, 0, len(fields))
		for key := range fields {
			leftover = append(leftover, key)
		}
		slices.Sort(leftover)
		return upd, fmt.Sprintf("unknown field %q", leftover[0]), nil
	}
	return upd, "", nil
}

func consume(fields map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, present := fields[key]
	delete(fields, key)
	return raw, present
}
