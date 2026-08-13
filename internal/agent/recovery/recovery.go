// Package recovery derives the one-time code that proves physical access to a
// relay, the anchor the household owner-role recovery ceremony hangs off
// (docs/app-bearer-contract.md §3).
//
// # Why the code is DERIVED rather than minted
//
// `poolpilot-relay show-recovery` is a CLI an operator runs on the box, and it
// is strictly READ-ONLY — the running agent (a systemd DynamicUser service) is
// the sole writer of the state file, and a second writer racing it is how a
// pairing gets silently lost. See cmd/poolpilot-relay/showpairing.go, which
// makes the same promise for the same reason.
//
// That rules out the obvious design ("the CLI mints a code and stores its
// hash"), and it rules out asking the running agent for one (the operator holds
// no LAN-API bearer — not holding one is the whole reason they are running this
// command). What is left is derivation: the CLI and the agent compute the SAME
// code from material the state file already carries, independently, with no
// write on the printing side.
//
// # What the code actually proves
//
// Possession of the relay's LAN-API private key, i.e. read access to the state
// file, i.e. root or physical access to the machine. docs/pairing-trust.md
// already declares that the out-of-band trust root; this extends it from
// install-time pairing to a lifetime ownership anchor, which is exactly what
// docs/app-bearer-contract.md §3 asks for. The code itself is not a secret to be protected in
// transit so much as a proof that the person typing it was standing at the box
// within the last few minutes.
//
// # Single-use, despite deriving
//
// A derived code cannot be "consumed" by deleting a row, so consumption is
// recorded as a HIGH-WATER MARK instead: the agent stores the window index it
// last accepted and refuses anything at or below it (Verify's minWindow). That
// is monotonic, needs one integer of state, and — unlike a set of spent codes —
// cannot be defeated by replaying an OLDER still-in-skew code.
package recovery

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"strconv"
	"time"

	"github.com/ylabonte/poolpilot-relay/idgen"
)

// Window is how long one recovery code stays valid.
//
// Ten minutes matches the invite TTL on the cloud side, and for the same
// reason: the ceremony is "read the screen, walk to the phone, scan". Longer
// would widen the window in which a shoulder-surfed or screenshotted code is
// worth something; shorter would start failing honest users mid-walk.
const Window = 10 * time.Minute

// hkdfInfo domain-separates this derivation from every other use of the LAN-API
// key material. Versioned so a future change of scheme cannot be confused with
// this one — bump the suffix rather than silently altering the inputs, or a
// mixed-version fleet would derive codes that look valid and are not.
const hkdfInfo = "poolpilot-relay/recovery-code/v1"

// ErrNoKeyMaterial means the relay has not written its TLS identity yet — the
// agent's first-boot window. The caller (the CLI) reports it as "the relay
// hasn't started yet" rather than as a derivation failure, because that is what
// it means to the person reading it.
var ErrNoKeyMaterial = errors.New("recovery: relay has no TLS key material yet")

// WindowIndex is the window number containing t. Codes are keyed on it, and the
// agent stores the highest one it has accepted.
func WindowIndex(t time.Time) int64 { return t.UTC().Unix() / int64(Window/time.Second) }

// WindowEnd is when the code for window w stops being the CURRENT one. The CLI
// prints it so the operator knows how long they have; note Verify still accepts
// the previous window for skew, so this is the conservative number to show.
func WindowEnd(w int64) time.Time {
	return time.Unix((w+1)*int64(Window/time.Second), 0).UTC()
}

// Code derives the recovery code for a given window.
//
// keyPEM is the relay's LAN-API private key (state.TLS.KeyPEM) and agentID its
// public identity. The agent id is the HKDF salt rather than part of the
// secret: it is public (mDNS TXT, GET /v1/info), so it adds no entropy, but
// mixing it in means two relays that somehow shared key material would still
// derive different codes.
func Code(keyPEM, agentID string, window int64) (string, error) {
	if keyPEM == "" {
		return "", ErrNoKeyMaterial
	}
	key, err := hkdf.Key(sha256.New, []byte(keyPEM), []byte(agentID), hkdfInfo, sha256.Size)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	// Length-free but unambiguous: the window is a decimal integer and the
	// separator cannot occur in it, so no two (agentID, window) pairs collide.
	_, _ = mac.Write([]byte(agentID))
	_, _ = mac.Write([]byte("|"))
	_, _ = mac.Write([]byte(strconv.FormatInt(window, 10)))
	var raw [8]byte
	copy(raw[:], mac.Sum(nil))
	// Same "XXXX-XXXX" Crockford shape as an enrolment or invite code, because
	// it is typed by the same person into the same field. 32 divides 256, so the
	// byte→character mapping is unbiased.
	return idgen.CodeFromBytes(raw), nil
}

// CodeAt is Code for the window containing t — what the CLI prints.
func CodeAt(keyPEM, agentID string, t time.Time) (string, error) {
	return Code(keyPEM, agentID, WindowIndex(t))
}

// Verify checks a presented code against the windows valid at t and reports
// which window it matched.
//
// It accepts the CURRENT window and the one before it. That tolerance is not
// generosity: without it a code printed at 10:09:58 would be refused at
// 10:00:02, which reads to the user as "the code is broken" and drives them to
// re-run the command in a loop. Two windows bound the exposure at ~20 minutes.
//
// minWindow is the single-use guard: the agent passes (last accepted window +
// 1), so a code that has already been redeemed — and every OLDER code still
// inside the skew tolerance — is refused. Pass 0 to disable the check (only
// meaningful in tests).
//
// The comparison is constant-time. The codes are short and derived from a
// secret, so a timing oracle would leak them character by character to anyone
// who can reach /v1/pair on the LAN.
func Verify(keyPEM, agentID, presented string, t time.Time, minWindow int64) (int64, bool) {
	if presented == "" {
		return 0, false
	}
	current := WindowIndex(t)
	for _, w := range []int64{current, current - 1} {
		if w < minWindow {
			continue
		}
		want, err := Code(keyPEM, agentID, w)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(presented)) == 1 {
			return w, true
		}
	}
	return 0, false
}
