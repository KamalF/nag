// Command nag is the single binary behind every subcommand (§3).
//
// There are no command-line flags anywhere, on any subcommand: everything
// that varies is an environment variable (§5.1) or a TOML key (§5.3). The
// default subcommand lives in the Dockerfile's CMD ["serve"] (§10) and
// nowhere else — no argument here is the unknown-word case, never a default.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"

	// The scratch image has no tzdata (§2); timezone validation and preset
	// arithmetic need the embedded copy everywhere the binary runs.
	_ "time/tzdata"

	"github.com/KamalF/nag/internal/config"
)

// version is the only build-time variable (§10), overridden with
// -ldflags="-X main.version=...". It is printed by `nag version` and logged
// at boot; the "dev" default marks a hand-built binary.
var version = "dev"

const usage = `usage: nag <subcommand>

  nag serve          # the default, per the Dockerfile CMD
  nag genkeys        # emit a complete env file to stdout
  nag healthcheck    # exit 0 if the local instance is serving; for Docker HEALTHCHECK
  nag version        # build version + Go version to stdout, exit 0
  nag config check   # validate NAG_CONFIG and exit; 0 if a reload would accept it
  nag channel add|list|rm|enable|disable|test
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(stdout, "nag %s %s\n", version, runtime.Version())
		return 0
	case "genkeys":
		return runGenkeys(stdout, stderr)
	case "config":
		if len(args) != 2 || args[1] != "check" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return runConfigCheck(stdout, stderr)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
}

// configPath resolves NAG_CONFIG with its §5.1 default. (The full env
// table and per-subcommand requirements land with the boot-checks commit.)
func configPath() string {
	if p := os.Getenv("NAG_CONFIG"); p != "" {
		return p
	}
	return "/config/nag.toml"
}

// runConfigCheck validates NAG_CONFIG through the same code path as boot
// and prints either the resolved preset list in file order or the located
// error (§5.5). It never writes the default file — an absent config is an
// error here, not something to fix silently.
func runConfigCheck(stdout, stderr io.Writer) int {
	cfg, err := config.Check(configPath())
	if err != nil {
		fmt.Fprintf(stderr, "FATAL: %v\n", err)
		return 1
	}
	for _, p := range cfg.Presets {
		fmt.Fprintf(stdout, "%-16s %-20s %s\n", p.Key, p.Label, describePreset(p))
	}
	return 0
}

func describePreset(p config.Preset) string {
	var s string
	switch p.Kind {
	case "offset":
		s = "offset " + *p.Offset
	case "clock":
		s = "clock " + *p.At
		if p.Days != nil && *p.Days > 0 {
			s += fmt.Sprintf(" +%dd", *p.Days)
		}
	case "weekday":
		s = "weekday " + *p.Weekday + " " + *p.At
		if p.SameDayOK != nil && *p.SameDayOK {
			s += " (same day ok)"
		}
	}
	if p.Quick {
		s += " [quick]"
	}
	return s
}
