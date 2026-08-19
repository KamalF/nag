package httpapi

import "net/http"

// handleConfig is GET /api/config (§8.2): the client-visible projection
// only — {key,label,quick} per preset, never kind/offset/at/days/weekday/
// same_day_ok, the timezone, or retention_days. What this projection
// excludes is exactly what §5.5's client hash may not see.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	presets := make([]map[string]any, 0, len(s.cfg.Presets))
	for _, p := range s.cfg.Presets {
		presets = append(presets, map[string]any{
			"key":   p.Key,
			"label": p.Label,
			"quick": p.Quick,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config_version": s.configVersion,
		"vapid_public":   s.vapidPublic,
		"default_preset": s.cfg.General.DefaultPreset,
		"presets":        presets,
		"picker": map[string]any{
			"hour_min":     s.cfg.Picker.HourMin,
			"hour_max":     s.cfg.Picker.HourMax,
			"minute_step":  s.cfg.Picker.MinuteStep,
			"default_time": s.cfg.Picker.DefaultTime,
			"week_start":   s.cfg.Picker.WeekStart,
		},
	})
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
