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
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
}
