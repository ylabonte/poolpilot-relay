package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunCLI_NoArgsFallsThroughToAgent guards the systemd path: the service
// starts the binary with no arguments and expects the agent to run. runCLI must
// report handled=false (and write nothing) so main() falls through to run().
func TestRunCLI_NoArgsFallsThroughToAgent(t *testing.T) {
	var out, errb bytes.Buffer
	code, handled := runCLI(nil, &out, &errb)
	if handled {
		t.Fatalf("runCLI(nil) handled=true, want false so the agent runs")
	}
	if code != 0 {
		t.Fatalf("runCLI(nil) code=%d, want 0", code)
	}
	if out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("runCLI(nil) wrote output: stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// TestRunCLI_VersionPrintsVersionToStdout covers `version`, `--version`, `-v`.
func TestRunCLI_VersionPrintsVersionToStdout(t *testing.T) {
	orig := version
	version = "v9.9.9-test"
	defer func() { version = orig }()

	for _, arg := range []string{"version", "--version", "-v"} {
		t.Run(arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			code, handled := runCLI([]string{arg}, &out, &errb)
			if !handled || code != 0 {
				t.Fatalf("runCLI(%q) handled=%v code=%d, want handled=true code=0", arg, handled, code)
			}
			if strings.TrimSpace(out.String()) != "v9.9.9-test" {
				t.Errorf("runCLI(%q) stdout=%q, want the version alone", arg, out.String())
			}
			if errb.Len() != 0 {
				t.Errorf("runCLI(%q) wrote to stderr: %q", arg, errb.String())
			}
		})
	}
}

// TestRunCLI_HelpPrintsUsageToStdout covers `help`, `--help`, `-h`.
func TestRunCLI_HelpPrintsUsageToStdout(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			var out, errb bytes.Buffer
			code, handled := runCLI([]string{arg}, &out, &errb)
			if !handled || code != 0 {
				t.Fatalf("runCLI(%q) handled=%v code=%d, want handled=true code=0", arg, handled, code)
			}
			got := out.String()
			for _, want := range []string{"Usage:", "show-pairing", "show-recovery", "version", "--help"} {
				if !strings.Contains(got, want) {
					t.Errorf("runCLI(%q) help output missing %q; got:\n%s", arg, want, got)
				}
			}
			if errb.Len() != 0 {
				t.Errorf("runCLI(%q) wrote to stderr: %q", arg, errb.String())
			}
		})
	}
}

// TestRunCLI_UnknownCommandIsUsageError: an unrecognized argument must be a
// usage error (stderr + exit 2), never a silent fall-through that starts the
// agent — that was the pre-existing footgun this change closes.
func TestRunCLI_UnknownCommandIsUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	code, handled := runCLI([]string{"frobnicate"}, &out, &errb)
	if !handled {
		t.Fatal("runCLI(unknown) handled=false, want true so it does not fall through to the agent")
	}
	if code != 2 {
		t.Errorf("runCLI(unknown) code=%d, want 2", code)
	}
	if out.Len() != 0 {
		t.Errorf("runCLI(unknown) wrote to stdout %q, want the usage error on stderr only", out.String())
	}
	if !strings.Contains(errb.String(), "unknown command") || !strings.Contains(errb.String(), "Usage:") {
		t.Errorf("runCLI(unknown) stderr=%q, want an unknown-command usage error", errb.String())
	}
}
