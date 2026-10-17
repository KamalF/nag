package httpapi

import (
	"context"
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

	"github.com/KamalF/nag/internal/config"
	"github.com/KamalF/nag/internal/presets"
	"github.com/KamalF/nag/internal/store"
)

// §8.3: due_at must land in 2000-01-01 .. 2100-01-01 UTC. The check stops
// a curl typo or a millisecond timestamp from becoming a stored row. Read
// as seconds, a millisecond timestamp is a date around the year 56 600,
// and every formatter downstream must render it.
const (
	dueAtMin = 946684800
	dueAtMax = 4102444800
)

const maxExtraChannels = 16

// reminderResponse is the §8.2 reminder object: pushed_at is deliberately
// absent, text is raw, extra_channels is always an array, and the other
// optional fields are null when unset.
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

// handleCreateReminder is POST /api/reminders (§8.3). The body holds text,
// exactly one of preset/due_at, and optionally extra_channels. The answer
// is 201 + the reminder.
func (s *Server) handleCreateReminder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text          *string  `json:"text"`
		Preset        *string  `json:"preset"`
		DueAt         *int64   `json:"due_at"`
		ExtraChannels []string `json:"extra_channels"`
	}
	if errMessage := decodeBody(r, &body); errMessage != "" {
		writeError(w, http.StatusBadRequest, errMessage)
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
	dueAt, errMessage := resolveDueAt(s.live.Load(), body.Preset, body.DueAt, now)
	if errMessage != "" {
		writeError(w, http.StatusBadRequest, errMessage)
		return
	}

	channels, errMessage, err := s.canonicalChannels(r.Context(), body.ExtraChannels, nil)
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
	ServerTime     int64              `json:"server_time"`
	ConfigVersion  int                `json:"config_version"`
	OverdueCount   int                `json:"overdue_count"`
	PushSubscribed bool               `json:"push_subscribed"` // instance-wide, not per-device (§8.2)
	Overdue        []reminderResponse `json:"overdue"`
	Later          []reminderResponse `json:"later"`
}

// handleState is GET /api/state (§8.2). It reads `now` once, and every part
// of the response derives from it: server_time, overdue_count, and the list
// that each row lands in. So the list can never disagree with the badge.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	pending, err := s.store.ListPending(r.Context())
	if err != nil {
		s.log.Error("state: list reminders", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	subscriptions, err := s.store.SubscriptionCount(r.Context())
	if err != nil {
		s.log.Error("state: count subscriptions", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	resp := stateResponse{
		ServerTime:     now,
		ConfigVersion:  s.live.Load().Version,
		PushSubscribed: subscriptions > 0,
		Overdue:        []reminderResponse{},
		Later:          []reminderResponse{},
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
		w.WriteHeader(http.StatusNotModified) // the client reads the clock from the Date header (§9.3)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// stateETag hashes the response with server_time excluded (§8.2). That
// field changes every second. If the hash included it, the 304 never
// fired. The weak label (W/) is accurate, not a precaution. Two responses
// that share the tag are equivalent for every purpose that this client has,
// but they are not byte-identical, because server_time moved between them.
func stateETag(resp stateResponse) string {
	resp.ServerTime = 0
	raw, _ := json.Marshal(resp)
	sum := sha256.Sum256(raw)
	return `W/"` + hex.EncodeToString(sum[:8]) + `"`
}

// resolveDueAt applies §8.3's exactly-one-of rule and returns the due
// instant in Unix seconds. The server never falls back to default_preset.
// The client always resolves default_preset itself.
func resolveDueAt(snap *config.Snapshot, preset *string, dueAt *int64, now time.Time) (int64, string) {
	switch {
	case preset != nil && dueAt != nil:
		return 0, "send either preset or due_at, not both"
	case preset != nil:
		p, ok := findPreset(snap, *preset)
		if !ok {
			return 0, fmt.Sprintf("unknown preset %q — reload to get the current list", *preset)
		}
		due, err := presets.Evaluate(p, now, snap.Location)
		if err != nil {
			// With a validated config, this error cannot happen. If it
			// happens anyway, the message says what failed.
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

func findPreset(snap *config.Snapshot, key string) (config.Preset, bool) {
	for _, p := range snap.Presets {
		if p.Key == key {
			return p, true
		}
	}
	return config.Preset{}, false
}

// validateText applies §8.3: trimmed, non-empty after trimming, max 1000
// bytes, and no control characters. Newlines count as control characters,
// because the capture control is a single-line input. It returns the
// trimmed text, or a message that names the character class, not the byte.
// decodeBody checks that the body is valid UTF-8, because only the raw
// bytes can still show invalid UTF-8.
func validateText(raw string) (text, errMessage string) {
	text = strings.TrimSpace(raw)
	if text == "" {
		return "", "text must not be empty"
	}
	if len(text) > 1000 {
		return "", fmt.Sprintf("text is %d bytes, maximum is 1000", len(text))
	}
	for _, r := range text {
		// U+2028/2029 are line breaks too, without being category Cc
		if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) {
			return "", "text must not contain control characters, newlines included"
		}
	}
	return text, ""
}

// canonicalChannels validates and canonicalizes extra_channels (§8.3):
// max 16 names, de-duplicated and sorted so that a set of names has exactly
// one stored form. The store enforces that invariant again when it writes.
// Every name must exist in channels, disabled ones included, because
// existence is what matters. The exception is the names in carried, which
// are already stored on the row. This is §8.3's PATCH asymmetry: a PATCH
// can keep an orphan name, but it cannot add one.
func (s *Server) canonicalChannels(ctx context.Context, names []string, carried []string) ([]string, string, error) {
	if len(names) == 0 {
		return nil, "", nil
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) > maxExtraChannels {
		return nil, fmt.Sprintf("extra_channels has %d names, maximum is %d", len(names), maxExtraChannels), nil
	}
	channels, err := s.store.ListChannels(ctx)
	if err != nil {
		return nil, "", err
	}
	known := make(map[string]struct{}, len(channels))
	for _, c := range channels {
		known[c.Name] = struct{}{}
	}
	for _, name := range names {
		if _, exists := known[name]; !exists && !slices.Contains(carried, name) {
			return nil, fmt.Sprintf("unknown channel %q", name), nil
		}
	}
	return names, "", nil
}
