package web

import (
	"encoding/json"
	"image"
	_ "image/png"
	"testing"
)

// §9.3 pins the minimum content of the manifest. An install reads every value
// in it once and never reads it again. A wrong `scope` or `display` gives a
// browser tab that pretends to be an app, on a phone, three weeks later.
func TestManifestPinnedValues(t *testing.T) {
	raw, err := Files.ReadFile("manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		ID              string `json:"id"`
		StartURL        string `json:"start_url"`
		Scope           string `json:"scope"`
		Display         string `json:"display"`
		BackgroundColor string `json:"background_color"`
		ThemeColor      string `json:"theme_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest is not json: %v", err)
	}
	for _, got := range []struct{ field, value, want string }{
		{"name", m.Name, "Nag"},
		{"short_name", m.ShortName, "Nag"},
		{"id", m.ID, "/"},
		{"start_url", m.StartURL, "/"},
		{"scope", m.Scope, "/"},
		{"display", m.Display, "standalone"},
		// theme_color can only hold one value, so it takes the dark
		// --surface. This is the single place that the OS-follows-theme rule
		// of §9.1 does not reach.
		{"background_color", m.BackgroundColor, "#212A2E"},
		{"theme_color", m.ThemeColor, "#212A2E"},
	} {
		if got.value != got.want {
			t.Errorf("%s = %q, want %q", got.field, got.value, got.want)
		}
	}

	want := map[string]string{
		"/icons/192.png":          "192x192",
		"/icons/512.png":          "512x512",
		"/icons/512-maskable.png": "512x512 maskable",
	}
	if len(m.Icons) != len(want) {
		t.Fatalf("manifest lists %d icons, want %d", len(m.Icons), len(want))
	}
	for _, icon := range m.Icons {
		sizes := icon.Sizes
		if icon.Purpose != "" {
			sizes += " " + icon.Purpose
		}
		if wantSizes, ok := want[icon.Src]; !ok {
			t.Errorf("icon %s is unexpected or listed twice", icon.Src)
		} else if wantSizes != sizes {
			t.Errorf("icon %s = %q, want %q", icon.Src, sizes, wantSizes)
		}
		delete(want, icon.Src) // a duplicate must not stand in for a missing icon
		if icon.Type != "image/png" {
			t.Errorf("icon %s type = %q, want image/png", icon.Src, icon.Type)
		}
	}
}

// The manifest, the icon and badge slots of the service worker, and the
// canvas favicon name the four PNGs (§9.3), all by literal path and size. A
// file that is missing, or right in name and wrong in pixels, fails at
// whichever of those three it reaches first.
func TestIconsAreEmbeddedAtTheirNamedSizes(t *testing.T) {
	for name, want := range map[string]int{
		"icons/32.png":           32,
		"icons/192.png":          192,
		"icons/512.png":          512,
		"icons/512-maskable.png": 512,
	} {
		f, err := Files.Open(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		cfg, format, err := image.DecodeConfig(f)
		f.Close()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if format != "png" || cfg.Width != want || cfg.Height != want {
			t.Errorf("%s is %s %dx%d, want png %dx%d", name, format, cfg.Width, cfg.Height, want, want)
		}
	}
	entries, err := Files.ReadDir("icons")
	if err != nil {
		t.Fatal(err)
	}
	// §9.3: exactly four. The directory is embedded whole, so a stray file
	// here ships in the binary.
	if len(entries) != 4 {
		t.Errorf("icons/ holds %d files, want exactly 4", len(entries))
	}
}

// §9.5: worker scope is path-based, so sw.js must sit at the root of the
// embedded tree. A nested worker cannot control the page. The failure is a
// silent registration under a scope that never sees a navigation.
func TestServiceWorkerIsEmbeddedAtTheRoot(t *testing.T) {
	if _, err := Files.ReadFile("sw.js"); err != nil {
		t.Fatal(err)
	}
}
