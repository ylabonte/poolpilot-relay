package idgen

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// GUID returns a 128-bit random value as a 32-char hex string (a valid DNS label).
func GUID() string {
	var b [16]byte
	mustRead(b[:])
	return hex.EncodeToString(b[:])
}

// Token returns a 256-bit random hex string for per-relay frpc auth.
func Token() string {
	var b [32]byte
	mustRead(b[:])
	return hex.EncodeToString(b[:])
}

// crockford excludes I, L, O, U to avoid human transcription errors.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// EnrollmentCode returns a human-friendly "XXXX-XXXX" one-time code. Also the
// shape used by household invite codes (internal/api/invites.go).
func EnrollmentCode() string {
	var raw [8]byte
	mustRead(raw[:])
	return CodeFromBytes(raw)
}

// CodeFromBytes renders 8 bytes in EnrollmentCode's "XXXX-XXXX" Crockford
// shape, for the one caller that must DERIVE a code rather than draw a fresh
// random one: the relay's recovery code (internal/agent/recovery) is an HMAC
// the printing CLI and the verifying agent compute independently, so it cannot
// come from crypto/rand and must still look and type like every other code.
//
// The mapping is unbiased: len(crockford) is 32 and divides 256 exactly, so
// every character is hit by exactly eight byte values.
func CodeFromBytes(raw [8]byte) string {
	out := make([]byte, 0, 9)
	for i, v := range raw {
		if i == 4 {
			out = append(out, '-')
		}
		out = append(out, crockford[int(v)%len(crockford)])
	}
	return string(out)
}

const base62 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// pushoverShaped returns prefix + 29 unbiased base62 chars (30 chars total),
// matching Pushover's key shape so firmware-side validation never trips.
func pushoverShaped(prefix byte) string {
	out := make([]byte, 30)
	out[0] = prefix
	var buf [1]byte
	for i := 1; i < 30; {
		mustRead(buf[:])
		if buf[0] >= 248 { // 4*62: reject to avoid modulo bias
			continue
		}
		out[i] = base62[buf[0]%62]
		i++
	}
	return string(out)
}

func IngestID() string     { return pushoverShaped('u') } // Pushover user keys start with 'u'
func IngestSecret() string { return pushoverShaped('a') } // Pushover app tokens start with 'a'

func mustRead(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
}

// HashToken is the at-rest form of every bearer credential: the DB stores only
// sha256(token) so a dump/backup leak cannot impersonate relays. Hash the
// presented plaintext before any lookup.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
