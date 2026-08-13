// show-recovery prints the relay's owner-recovery QR: the same pairing
// Universal Link show-pairing emits (SPKI pin + agent id), plus a one-time
// recovery code the AGENT verifies locally at /v1/pair. Redeeming it yields an
// OWNER app bearer for the household this relay belongs to — the physical-
// access anchor of plan decision 2.
//
// Like show-pairing it is STRICTLY READ-ONLY: no state write, no certificate
// generation, no cloud call. The code is derived, not minted, precisely so this
// stays true — see internal/agent/recovery's package doc for why that matters
// and how single use survives it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/ylabonte/poolpilot-relay/internal/agent/recovery"
	"github.com/ylabonte/poolpilot-relay/internal/agent/state"
	"github.com/ylabonte/poolpilot-relay/internal/agent/tlscert"
)

// notEnrolledMessage is printed when the relay holds no cloud credentials. The
// recovery ceremony brokers a voucher through the control plane using the
// relay's own frpc_token, so an un-enrolled relay has nothing to recover INTO —
// and telling the operator that beats printing a code that will fail at the
// last step for a reason they cannot see.
const notEnrolledMessage = "this relay isn't enrolled with the cloud yet, so there is no household to recover — run the normal pairing flow instead (`poolpilot-relay show-pairing`)"

// runShowRecovery implements `poolpilot-relay show-recovery`. It returns a
// process exit code rather than calling os.Exit itself so it stays testable.
func runShowRecovery(args []string) int {
	fs := flag.NewFlagSet("show-recovery", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: poolpilot-relay show-recovery")
		fmt.Fprintln(os.Stderr, "\nPrints a one-time code that lets a phone re-take the OWNER role for")
		fmt.Fprintln(os.Stderr, "this relay's household. Anyone who can run this command already has")
		fmt.Fprintln(os.Stderr, "root on the relay, which is the trust root the ceremony rests on.")
		fmt.Fprintln(os.Stderr, "Read-only — never starts or configures the agent.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	st, err := loadStateReadOnly(state.Path())
	if err != nil {
		fmt.Fprintln(os.Stderr, notReadyMessage)
		return 1
	}
	if !st.Enrolled() {
		fmt.Fprintln(os.Stderr, notEnrolledMessage)
		return 1
	}

	cert, err := tlscert.Load([]byte(st.TLS.CertPEM), []byte(st.TLS.KeyPEM))
	if err != nil {
		fmt.Fprintf(os.Stderr, "show-recovery: parse LAN-API certificate: %v\n", err)
		return 1
	}
	fp, err := tlscert.SPKIFingerprint(cert)
	if err != nil {
		fmt.Fprintf(os.Stderr, "show-recovery: compute fingerprint: %v\n", err)
		return 1
	}

	now := time.Now()
	window := recovery.WindowIndex(now)
	code, err := recovery.Code(st.TLS.KeyPEM, st.AgentID, window)
	if err != nil {
		if errors.Is(err, recovery.ErrNoKeyMaterial) {
			fmt.Fprintln(os.Stderr, notReadyMessage)
			return 1
		}
		fmt.Fprintf(os.Stderr, "show-recovery: derive recovery code: %v\n", err)
		return 1
	}

	recoveryURL := buildRecoveryURL(pairURLBase(), fp, st.AgentID, code)
	qrterminal.GenerateHalfBlock(recoveryURL, qrterminal.M, os.Stdout)
	fmt.Println()
	fmt.Println("Scan with the phone that should become the household owner, or paste this URL")
	fmt.Println("into the PoolPilot app:")
	fmt.Println("  " + recoveryURL)
	fmt.Println()
	fmt.Println("Manual entry — pair with this relay, then enter the recovery code:")
	fmt.Println("  fingerprint:   " + fp)
	fmt.Println("  recovery code: " + code)
	fmt.Println()
	// The conservative number: Verify also accepts the previous window, but a
	// user told "10:20" who succeeds at 10:24 is pleasantly surprised, whereas
	// one told "10:30" who fails at 10:29 concludes the feature is broken.
	fmt.Printf("Valid until %s (about %s from now). Re-run this command for a fresh code.\n",
		recovery.WindowEnd(window).Local().Format("15:04:05"),
		recovery.WindowEnd(window).Sub(now).Round(time.Second))
	fmt.Println("Single use: redeeming it invalidates this code and every older one.")

	return 0
}

// buildRecoveryURL is buildPairURL plus the recovery code, so one scan carries
// everything the app needs: which relay (agent), how to trust its TLS (fp), and
// what to present at /v1/pair (rc). Same url.Values encoding, and for the same
// reason — the fingerprint routinely contains '/', '+' and '='.
//
// The code rides in the link deliberately. It is worth nothing without LAN
// reach to this specific relay, it expires in minutes, and the alternative —
// making the user type it after scanning — is exactly the transcription step
// the QR exists to remove.
func buildRecoveryURL(base, fp, agentID, code string) string {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		u, _ = url.Parse(defaultPairURLBase)
	}
	u = u.JoinPath("pair")

	q := url.Values{}
	q.Set("v", "1")
	q.Set("fp", fp)
	q.Set("agent", agentID)
	q.Set("rc", code)
	u.RawQuery = q.Encode()

	return u.String()
}

// pairURLBase resolves PAIR_URL_BASE once for both subcommands.
func pairURLBase() string {
	if base := os.Getenv("PAIR_URL_BASE"); base != "" {
		return base
	}
	return defaultPairURLBase
}
