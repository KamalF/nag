package main

import (
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The scratch image has no system zoneinfo, so the binary carries its own
// (§2). Every test still passes without the import, because the host has
// zoneinfo: only the image fails, at boot. So the import itself is the check.
func TestMainEmbedsTheTimezoneDatabase(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path == "time/tzdata" {
			return
		}
	}
	t.Error(`main.go does not import "time/tzdata"`)
}

// The build context holds nag.env (the VAPID private key) and data/ (the
// database). Without these exclusions, COPY . . puts both in the cached
// builder layer (§10).
func TestDockerignoreKeepsSecretsOutOfTheBuild(t *testing.T) {
	raw, err := os.ReadFile("../../.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	for _, want := range []string{"nag.env", ".env", "data/", "config/", "*.db*"} {
		if !slices.Contains(lines, want) {
			t.Errorf(".dockerignore does not exclude %s", want)
		}
	}
}
