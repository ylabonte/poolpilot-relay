// show-pairing prints the relay's pairing QR (a Universal Link carrying the
// LAN-API's SPKI pin) plus the bare fingerprint for manual entry. It is
// STRICTLY READ-ONLY: it never mints state, never generates a certificate,
// and never writes to STATE_PATH — the running agent (systemd DynamicUser)
// remains the sole writer of that file. See loadStateReadOnly.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tlscert"
)

// defaultPairURLBase is the Universal Link host; override via PAIR_URL_BASE
// (e.g. for a staging domain).
const defaultPairURLBase = "https://pair.poolpilot.eu"

// notReadyMessage is printed whenever the relay's TLS identity isn't on disk
// yet — either the agent hasn't started at all, or it's still in the brief
// first-boot window before it persists the generated certificate. Shared by
// show-pairing and show-recovery, hence "this command" rather than naming one
// of them: printing the wrong subcommand back at an operator who just typed the
// other is a small thing that reads as the tool being confused.
const notReadyMessage = "relay hasn't started yet — check `systemctl status poolpilot-relay`, then re-run this command"

// certPollInterval/certPollTimeout bound how long show-pairing waits for an
// already-running agent to finish writing its first-boot TLS material.
const (
	certPollInterval = 200 * time.Millisecond
	certPollTimeout  = 10 * time.Second
)

// runShowPairing implements the `poolpilot-relay show-pairing` subcommand. It
// returns a process exit code rather than calling os.Exit itself so it stays
// testable.
func runShowPairing(args []string) int {
	fs := flag.NewFlagSet("show-pairing", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: poolpilot-relay show-pairing")
		fmt.Fprintln(os.Stderr, "\nPrints this relay's pairing QR (Universal Link) and its bare")
		fmt.Fprintln(os.Stderr, "fingerprint for manual entry. Read-only — never starts or configures")
		fmt.Fprintln(os.Stderr, "the agent.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path := state.Path()
	st, err := loadStateReadOnly(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, notReadyMessage)
		return 1
	}

	cert, err := tlscert.Load([]byte(st.TLS.CertPEM), []byte(st.TLS.KeyPEM))
	if err != nil {
		fmt.Fprintf(os.Stderr, "show-pairing: parse LAN-API certificate: %v\n", err)
		return 1
	}
	fp, err := tlscert.SPKIFingerprint(cert)
	if err != nil {
		fmt.Fprintf(os.Stderr, "show-pairing: compute fingerprint: %v\n", err)
		return 1
	}

	pairURL := buildPairURL(pairURLBase(), fp, st.AgentID)

	qrterminal.GenerateHalfBlock(pairURL, qrterminal.M, os.Stdout)
	fmt.Println()
	fmt.Println("Scan with your phone's camera, or paste this URL into the PoolPilot app:")
	fmt.Println("  " + pairURL)
	fmt.Println()
	fmt.Println("Manual-entry fingerprint (if you can't scan or paste):")
	fmt.Println("  " + fp)

	return 0
}

// buildPairURL constructs the Universal Link the app scans/opens to pair:
// <base>/pair?v=1&fp=<spki-fingerprint>&agent=<agentID>. fp routinely
// contains '/' (the "sha256/" prefix) and base64 '+'/'=' padding, so the
// query is built via url.Values.Encode() rather than string concatenation to
// guarantee correct percent-encoding.
func buildPairURL(base, fp, agentID string) string {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Malformed PAIR_URL_BASE override — fall back to the real default
		// rather than emitting a broken link.
		u, _ = url.Parse(defaultPairURLBase)
	}
	u = u.JoinPath("pair")

	q := url.Values{}
	q.Set("v", "1")
	q.Set("fp", fp)
	q.Set("agent", agentID)
	u.RawQuery = q.Encode()

	return u.String()
}

// loadStateReadOnly reads and decodes the relay's state document without
// ever creating or modifying it — unlike state.Open, which mints a fresh
// state file (and a new agent identity) when STATE_PATH doesn't exist yet.
// That behavior is correct for the agent itself but wrong here: show-pairing
// must never race the running agent's own first-boot write, nor fabricate an
// identity for a relay that hasn't booted.
//
// The v1 and v2 on-disk schemas both use "agent_id" and "tls" as their
// top-level field names for exactly the two fields this command needs, so
// decoding straight into state.State is correct for either schema — no
// version-aware migration is needed for a read-only peek.
func loadStateReadOnly(path string) (state.State, error) {
	if _, err := os.Stat(path); err != nil {
		return state.State{}, fmt.Errorf("state file %s: %w", path, err)
	}

	deadline := time.Now().Add(certPollTimeout)
	for {
		raw, err := os.ReadFile(path)
		if err != nil {
			return state.State{}, fmt.Errorf("read state file %s: %w", path, err)
		}
		var st state.State
		if err := json.Unmarshal(raw, &st); err != nil {
			return state.State{}, fmt.Errorf("state file %s: %w", path, err)
		}
		if st.TLS.CertPEM != "" {
			return st, nil
		}
		if time.Now().After(deadline) {
			return state.State{}, fmt.Errorf("state file %s: TLS certificate not yet written after %s", path, certPollTimeout)
		}
		time.Sleep(certPollInterval)
	}
}
