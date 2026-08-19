package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestConfigProjection(t *testing.T) {
	h := testServer(t, nil).Handler()
	rec := authed(t, h, http.MethodGet, "/api/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config = %d, want 200", rec.Code)
	}

	var cfg struct {
		ConfigVersion int              `json:"config_version"`
		VAPIDPublic   string           `json:"vapid_public"`
		DefaultPreset string           `json:"default_preset"`
		Presets       []map[string]any `json:"presets"`
		Picker        map[string]any   `json:"picker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.VAPIDPublic != testVAPIDPublic || cfg.DefaultPreset != "30min" || cfg.ConfigVersion == 0 {
		t.Errorf("config = %+v", cfg)
	}
	if len(cfg.Presets) != 2 || cfg.Presets[0]["key"] != "30min" || cfg.Presets[1]["key"] != "tomorrow" {
		t.Errorf("presets = %v, want file order", cfg.Presets)
	}
	for _, p := range cfg.Presets {
		if len(p) != 3 {
			t.Errorf("preset %v carries more than {key,label,quick}", p)
		}
	}
	// the §5.5 two-hash argument: none of these may reach the client
	for _, forbidden := range []string{"kind", "offset", "\"at\"", "days", "weekday",
		"same_day_ok", "timezone", "retention_days"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("projection leaks %s: %s", forbidden, rec.Body.String())
		}
	}
	for _, key := range []string{"hour_min", "hour_max", "minute_step", "default_time", "week_start"} {
		if _, ok := cfg.Picker[key]; !ok {
			t.Errorf("picker block missing %s", key)
		}
	}
}

func TestConfigVersionInBothEndpoints(t *testing.T) {
	h := testServer(t, nil).Handler()
	var fromConfig, fromState struct {
		ConfigVersion int `json:"config_version"`
	}
	json.Unmarshal(authed(t, h, http.MethodGet, "/api/config").Body.Bytes(), &fromConfig)
	json.Unmarshal(authed(t, h, http.MethodGet, "/api/state").Body.Bytes(), &fromState)
	if fromConfig.ConfigVersion != fromState.ConfigVersion || fromConfig.ConfigVersion == 0 {
		t.Errorf("config_version: /api/config = %d, /api/state = %d — must be the same counter",
			fromConfig.ConfigVersion, fromState.ConfigVersion)
	}
}

func TestChannelsEndpoint(t *testing.T) {
	s, dbPath := testServerWithDB(t, nil)
	h := s.Handler()

	t.Run("empty table → []", func(t *testing.T) {
		rec := authed(t, h, http.MethodGet, "/api/channels")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Errorf("GET /api/channels = %d %q, want 200 []", rec.Code, rec.Body.String())
		}
	})

	t.Run("name order, no url field", func(t *testing.T) {
		insertChannel(t, dbPath, "zulip", true)
		insertChannel(t, dbPath, "ntfy", false)
		rec := authed(t, h, http.MethodGet, "/api/channels")
		var channels []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &channels); err != nil {
			t.Fatal(err)
		}
		if len(channels) != 2 || channels[0]["name"] != "ntfy" || channels[1]["name"] != "zulip" {
			t.Errorf("channels = %v, want ntfy then zulip", channels)
		}
		if channels[0]["enabled"] != false || channels[1]["enabled"] != true {
			t.Errorf("enabled flags wrong: %v", channels)
		}
		if strings.Contains(rec.Body.String(), "url") || strings.Contains(rec.Body.String(), "ntfy://") {
			t.Errorf("channel URL leaked: %s", rec.Body.String())
		}
	})
}
