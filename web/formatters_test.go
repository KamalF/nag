package web

import (
	"strings"
	"testing"
)

// §9.11: the worker carries its own copy of the short formatter, because
// sw.js cannot import from app.js. Thresholds and units are exactly what
// drifts silently between two copies. Each shared function must be
// byte-identical in both files.
func TestWorkerFormatterMatchesThePage(t *testing.T) {
	page := readFile(t, "app.js")
	worker := readFile(t, "sw.js")
	for _, decl := range []string{
		"const clockFormat =",
		"const weekdayFormat =",
		"const dayMonthFormat =",
		"const dayMonthYearFormat =",
		"function whenText(dueAt) {",
		"function calendarDaysBetween(from, to) {",
		"function count(n, unit) {",
	} {
		p, w := declaration(page, decl), declaration(worker, decl)
		if p == "" || w == "" {
			t.Errorf("%q missing from app.js or sw.js", decl)
			continue
		}
		if p != w {
			t.Errorf("%q differs between app.js and sw.js:\napp.js:\n%s\nsw.js:\n%s", decl, p, w)
		}
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := Files.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// declaration returns a top-level declaration starting with start: a
// function up to its closing brace at column 0, a const up to its line end.
func declaration(src, start string) string {
	i := strings.Index(src, "\n"+start)
	if i < 0 {
		return ""
	}
	rest := src[i+1:]
	end := "\n"
	if strings.HasPrefix(start, "function") {
		end = "\n}\n"
	}
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j+len(end)]
}
