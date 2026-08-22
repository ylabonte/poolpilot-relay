package main

import (
	"fmt"
	"io"
)

// runCLI handles the argument-driven modes that short-circuit the agent: the
// show-* subcommands, `version`, and `help` (each with the conventional flag
// spellings). It returns handled=false ONLY when there is no leading argument —
// the normal `poolpilot-relay` service start — so main() falls through to run().
//
// An unrecognized argument is a usage error (stderr + exit 2), not a silent
// fall-through into the agent: starting a long-running daemon because someone
// mistyped a subcommand is a surprising footgun. The show-* subcommands keep
// writing to os.Stdout themselves (they render a QR code); stdout/stderr are
// passed in so version/help/usage stay testable without capturing the process's
// real streams.
func runCLI(args []string, stdout, stderr io.Writer) (code int, handled bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "show-pairing":
		return runShowPairing(args[1:]), true
	case "show-recovery":
		return runShowRecovery(args[1:]), true
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return 0, true
	case "help", "--help", "-h":
		writeUsage(stdout)
		return 0, true
	default:
		fmt.Fprintf(stderr, "poolpilot-relay: unknown command %q\n\n", args[0])
		writeUsage(stderr)
		return 2, true
	}
}

// writeUsage prints the top-level help. It documents the subcommands and the two
// conventional flags; the agent's full environment surface lives in the package
// doc (main.go) and the deployed config file, so it is pointed to rather than
// duplicated here.
func writeUsage(w io.Writer) {
	fmt.Fprint(w, `poolpilot-relay — the PoolPilot Relay edge agent

With no arguments it runs the agent (normally started by the systemd service).

Usage:
  poolpilot-relay                run the agent
  poolpilot-relay show-pairing   print this device's pairing QR code and fingerprint
  poolpilot-relay show-recovery  print a one-time owner-recovery code
  poolpilot-relay version        print the version and exit
  poolpilot-relay help           print this help and exit

Flags:
  -v, --version                  print the version and exit
  -h, --help                     print this help and exit

Configuration is read from the environment (see /etc/poolpilot-relay/config).
Documentation: https://github.com/ylabonte/poolpilot-relay
`)
}
