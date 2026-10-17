package web

import (
	"regexp"
	"strings"
	"testing"
)

// The token table of §9.1, byte for byte, both sets. The light set has
// deliberate deviations from a straight inversion. That is exactly what a
// hand-copied stylesheet gets wrong.
func TestCSSTokensMatchTheSpecTable(t *testing.T) {
	css := readFile(t, "app.css")
	darkAt := strings.Index(css, "@media (prefers-color-scheme: dark)")
	if darkAt < 0 {
		t.Fatal("no prefers-color-scheme: dark block")
	}
	light, dark := css[:darkAt], css[darkAt:]

	for _, tok := range []struct{ name, dark, light string }{
		{"--surface", "#212A2E", "#EFF1F0"},
		{"--surface-lift", "#263034", "#F7F8F8"},
		{"--sheet", "#1C2428", "#FFFFFF"},
		{"--field", "#1A2226", "#FFFFFF"},
		{"--ink", "#E4E9EA", "#1E282C"},
		{"--ink-quiet", "#8DA0A7", "#5C6C72"},
		{"--ink-faint", "#5D7078", "#97A3A7"},
		{"--edge", "#334147", "#D3DAD9"},
		{"--late", "#E0A33E", "#96620F"},
		{"--late-ink", "#212A2E", "#FFFFFF"},
		{"--chip", "#2C383D", "#E2E6E5"},
		{"--chip-ink", "#C2CED2", "#3C4A4E"},
		{"--scrim", "rgba(8,12,14,.62)", "rgba(30,40,44,.42)"},
	} {
		if got := tokenValue(light, tok.name); got != tok.light {
			t.Errorf("light %s = %q, want %q", tok.name, got, tok.light)
		}
		if got := tokenValue(dark, tok.name); got != tok.dark {
			t.Errorf("dark %s = %q, want %q", tok.name, got, tok.dark)
		}
	}
	// nothing sets a data-theme attribute, so a rule must not be scoped to one
	// (§9.1)
	if strings.Contains(css, "[data-theme") {
		t.Error("app.css scopes a rule to data-theme")
	}
}

func tokenValue(block, name string) string {
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `:\s*([^;]+);`).FindStringSubmatch(block)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// §9.7: style-src 'self' blocks a style="…" attribute parsed from HTML
// exactly as it blocks a <style> block. So the markup carries neither.
func TestIndexHasNoInlineStyle(t *testing.T) {
	html := readFile(t, "index.html")
	if strings.Contains(html, "style=") || strings.Contains(html, "<style") {
		t.Error("index.html carries an inline style")
	}
}
