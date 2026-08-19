package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/KamalF/nag/internal/config"
	"github.com/KamalF/nag/internal/presets"
	"github.com/KamalF/nag/internal/store"
)

// §8.3: due_at must land in 2000-01-01 .. 2100-01-01 UTC — the check stops
// a curl typo or a millisecond timestamp from becoming a row every
// formatter downstream renders around the year 56 600.
const (
	dueAtMin = 946684800
	dueAtMax = 4102444800
)

const maxExtraChannels = 16

// reminderResponse is the §8.2 reminder object: pushed_at deliberately
// absent, text raw, extra_channels always an array, the rest null when
// unset.
type reminderResponse struct {
	ID            int64    `json:"id"`
	Text          string   `json:"text"`
	DueAt         int64    `json:"due_at"`
	NotifiedAt    *int64   `json:"notified_at"`
	DoneAt        *int64   `json:"done_at"`
	ExtraChannels []string `json:"extra_channels"`
	DeliveryError *string  `json:"delivery_error"`
}

func toReminderResponse(r store.Reminder) reminderResponse {
	channels := r.ExtraChannels
	if channels == nil {
		channels = []string{}
	}
	return reminderResponse{
		ID:            r.ID,
		Text:          r.Text,
		DueAt:         r.DueAt,
		NotifiedAt:    r.NotifiedAt,
		DoneAt:        r.DoneAt,
		ExtraChannels: channels,
		DeliveryError: r.DeliveryError,
	}
}

// handleCreateReminder is POST /api/reminders (§8.3): text plus exactly
// one of preset/due_at, optional extra_channels. 201 + the reminder.
func (s *Server) handleCreateReminder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text          *string  `json:"text"`
		Preset        *string  `json:"preset"`
		DueAt         *int64   `json:"due_at"`
		ExtraChannels []string `json:"extra_channels"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "malformed json body")
		return
	}
	if body.Text == nil {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	text, errMessage := validateText(*body.Text)
	if errMessage != "" {
		writeError(w, http.StatusBadRequest, errMessage)
		return
	}

	now := time.Now()
	dueAt, errMessage := s.resolveDueAt(body.Preset, body.DueAt, now)
	if errMessage != "" {
		writeError(w, http.StatusBadRequest, errMessage)
		return
	}

	channels, errMessage, err := s.canonicalChannels(r, body.ExtraChannels, nil)
	if err != nil {
		s.log.Error("create reminder: read channels", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	if errMessage != "" {
		writeError(w, http.StatusBadRequest, errMessage)
		return
	}

	rem, err := s.store.CreateReminder(r.Context(), text, dueAt, channels, now.Unix())
	if err != nil {
		s.log.Error("create reminder", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	setLogID(r, strconv.FormatInt(rem.ID, 10))
	writeJSON(w, http.StatusCreated, toReminderResponse(rem))
}

type stateResponse struct {
	ServerTime    int64              `json:"server_time"`
	ConfigVersion int                `json:"config_version"`
	OverdueCount  int                `json:"overdue_count"`
	Overdue       []reminderResponse `json:"overdue"`
	Later         []reminderResponse `json:"later"`
}

// handleState is GET /api/state (§8.2). `now` is read once and every part
// of the response derives from it — server_time, overdue_count, and which
// list each row lands in — so the list can never disagree with the badge.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	pending, err := s.store.ListPending(r.Context())
	if err != nil {
		s.log.Error("state: list reminders", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	resp := stateResponse{
		ServerTime:    now,
		ConfigVersion: s.configVersion,
		Overdue:       []reminderResponse{},
		Later:         []reminderResponse{},
	}
	for _, rem := range pending { // already sorted by due_at, id (§8.2)
		if rem.DueAt <= now {
			resp.Overdue = append(resp.Overdue, toReminderResponse(rem))
		} else {
			resp.Later = append(resp.Later, toReminderResponse(rem))
		}
	}
	resp.OverdueCount = len(resp.Overdue)

	etag := stateETag(resp)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified) // client reads the clock from Date (§9.3)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// stateETag hashes the response with server_time excluded (§8.2): that
// field moves every second, and hashing it would mean the 304 never once
// fires. Weak (W/) is the accurate label, not a hedge: two responses
// sharing the tag are equivalent for every purpose this client has and
// are not byte-identical, since server_time has moved between them.
func stateETag(resp stateResponse) string {
	resp.ServerTime = 0
	raw, _ := json.Marshal(resp)
	sum := sha256.Sum256(raw)
	return `W/"` + hex.EncodeToString(sum[:8]) + `"`
}

// resolveDueAt applies §8.3's exactly-one-of rule and returns the due
// instant in Unix seconds. The server never falls back to default_preset —
// the client always resolves it.
func (s *Server) resolveDueAt(preset *string, dueAt *int64, now time.Time) (int64, string) {
	switch {
	case preset != nil && dueAt != nil:
		return 0, "send either preset or due_at, not both"
	case preset != nil:
		p, ok := s.findPreset(*preset)
		if !ok {
			return 0, fmt.Sprintf("unknown preset %q — reload to get the current list", *preset)
		}
		due, err := presets.Evaluate(p, now, s.loc)
		if err != nil {
			// unreachable on a validated config; answer honestly anyway
			return 0, fmt.Sprintf("preset %q cannot be evaluated", *preset)
		}
		return due.Unix(), ""
	case dueAt != nil:
		if *dueAt < dueAtMin || *dueAt > dueAtMax {
			return 0, fmt.Sprintf("due_at must be unix seconds between %d and %d", dueAtMin, dueAtMax)
		}
		return *dueAt, ""
	default:
		return 0, "one of preset or due_at is required"
	}
}

func (s *Server) findPreset(key string) (config.Preset, bool) {
	for _, p := range s.cfg.Presets {
		if p.Key == key {
			return p, true
		}
	}
	return config.Preset{}, false
}

// validateText applies §8.3: trimmed, non-empty after trimming, max 1000
// bytes, valid UTF-8, no control characters (newlines included — the
// capture control is a single-line input). Returns the trimmed text or a
// message naming the character class, not the byte.
func validateText(raw string) (text, errMessage string) {
	text = strings.TrimSpace(raw)
	if text == "" {
		return "", "text must not be empty"
	}
	if len(text) > 1000 {
		return "", fmt.Sprintf("text is %d bytes, maximum is 1000", len(text))
	}
	if !utf8.ValidString(text) {
		return "", "text is not valid utf-8"
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return "", "text must not contain control characters, newlines included"
		}
	}
	return text, ""
}

// canonicalChannels validates and canonicalises extra_channels (§8.3):
// max 16 names, de-duplicated and sorted before write so a set of names
// has exactly one stored form. Every name must exist in channels — except
// those in carried, the names already stored on the row (§8.3's PATCH
// asymmetry: an orphan can be carried forward, never introduced).
func (s *Server) canonicalChannels(r *http.Request, names []string, carried []string) ([]string, string, error) {
	if len(names) == 0 {
		return nil, "", nil
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) > maxExtraChannels {
		return nil, fmt.Sprintf("extra_channels has %d names, maximum is %d", len(names), maxExtraChannels), nil
	}
	known, err := s.store.ChannelNames(r.Context())
	if err != nil {
		return nil, "", err
	}
	for _, name := range names {
		if _, exists := known[name]; !exists && !slices.Contains(carried, name) {
			return nil, fmt.Sprintf("unknown channel %q", name), nil
		}
	}
	return names, "", nil
}
