package httpapi

import (
	"net/http"

	"github.com/KamalF/nag/internal/config"
)

// handleConfig is GET /api/config (§8.2): the client-visible projection
// only — {key,label,quick} per preset, never kind/offset/at/days/weekday/
// same_day_ok, the timezone, or retention_days. What this projection
// excludes is exactly what §5.5's client hash may not see.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	snap := s.live.Load() // one snapshot: the body and its version agree
	writeJSON(w, http.StatusOK, struct {
		ConfigVersion int `json:"config_version"`
		config.ClientView
	}{snap.Version, s.live.ClientView(snap.Config)})
}

// handleChannels is GET /api/channels (§8.2): [{name, enabled}] in name
// order — the client never re-sorts — and no URL in any form, not masked
// and not as a boolean.
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	channels, err := s.store.ListChannels(r.Context())
	if err != nil {
		s.log.Error("list channels", "error", err)
		writeError(w, http.StatusInternalServerError, "")
		return
	}
	out := make([]map[string]any, 0, len(channels))
	for _, c := range channels {
		out = append(out, map[string]any{"name": c.Name, "enabled": c.Enabled})
	}
	writeJSON(w, http.StatusOK, out)
}
