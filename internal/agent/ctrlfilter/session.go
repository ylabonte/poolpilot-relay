package ctrlfilter

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Web-session credentials for the ctrl vhost (issue #27's remaining item).
//
// The tunnel host <guid>.remote.<host> used to be a bare capability URL: the
// 32-hex GUID in the hostname was the ONLY gate, and it leaks through TLS SNI,
// Referer, server logs and browser history. These tokens add the second factor.
//
// The relay signs them itself with a secret that never leaves it — no control
// plane, no key distribution. Two kinds, same wire format and same verifier:
//
//   - a bootstrap TOKEN, short-lived and single-use, minted over the LAN API
//     (which the app reaches with the pairing bearer it already holds) and
//     redeemed once at GET /__session;
//   - a session COOKIE, longer-lived, set by that redemption and replayed by
//     the browser on every subsequent request.
//
// Both bind the controller GUID inside the signed payload, so neither can be
// moved to a different controller even by hand.
//
// This is deliberately viable only because of a product decision: without the
// app, nothing at the tunnel needs to work. There is no independent-browser
// flow to keep alive, which is what lets a cookie gate replace what would
// otherwise have to be a login form.
const (
	// CookieName is the session cookie the gate requires on every ctrl-vhost
	// request. The "pp_" prefix keeps it clear of anything a controller's own
	// firmware might set on the same host.
	CookieName = "pp_ctrl"

	// TokenTTL bounds how long a minted bootstrap token stays redeemable. Short
	// on purpose: the app mints one immediately before opening its WebView, so
	// the only thing a longer window would buy is replay surface.
	TokenTTL = 2 * time.Minute

	// CookieTTL bounds a redeemed session. Long enough that browsing the
	// controller UI is not interrupted, short enough that a cookie captured
	// from a device backup or a screenshot is not a permanent key.
	CookieTTL = 12 * time.Hour

	// tokenVersion prefixes every signed value. Verification refuses anything
	// else outright, so the format can be rotated without ambiguity.
	tokenVersion = "v1"
)

// ErrNoSessionKey is returned by the mint helpers when no session secret is
// configured. Minting must fail loudly rather than emit a value signed under
// an empty key, which would verify for anyone who guessed the scheme.
var ErrNoSessionKey = errors.New("ctrlfilter: no session key configured")

// SessionKey is the relay's HMAC secret for web sessions. It lives in the
// agent's state.json, is generated on first use, and never leaves the relay:
// it is not sent to the control plane, never logged, and never returned by any
// endpoint.
type SessionKey []byte

// mint builds "v1.<base64url(payload)>.<base64url(mac)>" where payload is
// "<guid>|<exp unix>|<nonce>". The nonce makes every mint distinct even for an
// identical (guid, exp) pair, which is what lets burnList retire a single
// bootstrap token, and makes the cookie value unguessable.
func mint(key SessionKey, guid string, now time.Time, ttl time.Duration) (string, error) {
	if len(key) == 0 {
		return "", ErrNoSessionKey
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	payload := guid + "|" + strconv.FormatInt(now.Add(ttl).Unix(), 10) + "|" + hex.EncodeToString(raw[:])
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	enc := base64.RawURLEncoding
	return tokenVersion + "." + enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// MintToken mints the short-lived, single-use bootstrap token the app hands to
// its WebView as ?t= on the /__session URL.
func MintToken(key SessionKey, guid string, now time.Time, ttl time.Duration) (string, error) {
	return mint(key, guid, now, ttl)
}

// MintCookie mints the longer-lived session cookie value handed out when a
// bootstrap token is redeemed.
func MintCookie(key SessionKey, guid string, now time.Time, ttl time.Duration) (string, error) {
	return mint(key, guid, now, ttl)
}

// VerifySigned checks a token or cookie against key, the expected controller
// guid, and now. It returns the value's nonce (which the caller burns for
// single-use bootstrap tokens) and whether the value is good.
//
// Every failure — wrong version, malformed encoding, bad MAC, expired, wrong
// controller, no key — returns the same (\"\", false). Callers must not
// distinguish them to the client: an attacker probing the gate should not
// learn which check tripped.
func VerifySigned(key SessionKey, value, guid string, now time.Time) (nonce string, ok bool) {
	if len(key) == 0 || value == "" {
		return "", false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] != tokenVersion {
		return "", false
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	gotMAC, err := enc.DecodeString(parts[2])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	// Constant-time: the MAC is checked before anything in the payload is
	// trusted, so a forged payload never reaches the parsing below.
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return "", false
	}
	fields := strings.Split(string(payload), "|")
	if len(fields) != 3 {
		return "", false
	}
	if !hmac.Equal([]byte(fields[0]), []byte(guid)) {
		return "", false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || !now.Before(time.Unix(exp, 0)) {
		return "", false
	}
	return fields[2], true
}

// burnList retires bootstrap-token nonces so a captured ?t= value cannot be
// redeemed twice. Entries are dropped once their own expiry passes, so the map
// stays bounded by how many tokens can be minted within TokenTTL rather than
// growing forever. The zero value is ready to use.
//
// It is in-memory on purpose: a relay restart forgets burned nonces, but every
// token it could accept afterwards has a TokenTTL of at most two minutes, and
// redeeming one still requires having captured it in that window.
type burnList struct {
	mu    sync.Mutex
	nonce map[string]time.Time
}

// use records nonce as spent and reports whether it was still fresh. Expired
// entries are pruned on every call.
func (b *burnList) use(nonce string, exp, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now)
	if _, spent := b.nonce[nonce]; spent {
		return false
	}
	if b.nonce == nil {
		b.nonce = make(map[string]time.Time)
	}
	b.nonce[nonce] = exp
	return true
}

// len reports how many unexpired entries are held, pruning first. Used by the
// tests to prove the map does not grow without bound.
func (b *burnList) len(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(now)
	return len(b.nonce)
}

func (b *burnList) pruneLocked(now time.Time) {
	for n, exp := range b.nonce {
		if !now.Before(exp) {
			delete(b.nonce, n)
		}
	}
}
