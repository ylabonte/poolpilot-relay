package ctrlfilter

import (
	"strings"
	"testing"
	"time"
)

var testKey = SessionKey("0123456789abcdef0123456789abcdef")

func TestVerifyAcceptsAFreshToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, err := MintToken(testKey, "abc123", now, TokenTTL)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, ok := VerifySigned(testKey, tok, "abc123", now.Add(time.Second)); !ok {
		t.Fatal("a fresh token must verify")
	}
}

func TestVerifyRejectsExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, _ := MintToken(testKey, "abc123", now, TokenTTL)
	if _, ok := VerifySigned(testKey, tok, "abc123", now.Add(TokenTTL+time.Second)); ok {
		t.Fatal("an expired token must be refused")
	}
}

// The GUID is inside the signed payload, so a token minted for one controller
// can never be replayed against another even if an attacker sets the cookie by
// hand (browsers already scope it per subdomain; this is the belt to that
// suspenders).
func TestVerifyRejectsAGUIDMismatch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, _ := MintToken(testKey, "controller-a", now, TokenTTL)
	if _, ok := VerifySigned(testKey, tok, "controller-b", now); ok {
		t.Fatal("a token minted for another controller must be refused")
	}
}

func TestVerifyRejectsATamperedPayloadAndAWrongKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, _ := MintToken(testKey, "abc123", now, TokenTTL)
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token shape = %q, want v1.payload.mac", tok)
	}
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]
	if _, ok := VerifySigned(testKey, tampered, "abc123", now); ok {
		t.Fatal("a tampered payload must be refused")
	}
	if _, ok := VerifySigned(SessionKey("ffffffffffffffffffffffffffffffff"), tok, "abc123", now); ok {
		t.Fatal("a token signed with another key must be refused")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, bad := range []string{"", "v1", "v1.", "v1.a", "v2.a.b", "....", "not-a-token"} {
		if _, ok := VerifySigned(testKey, bad, "abc123", now); ok {
			t.Fatalf("garbage %q must be refused", bad)
		}
	}
}

// An empty key must never verify anything — a relay that somehow reached the
// gate without a configured secret has to fail closed, not accept a MAC
// computed over the empty key.
func TestVerifyRejectsAnEmptyKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, err := MintToken(SessionKey(nil), "abc123", now, TokenTTL)
	if err == nil {
		t.Fatalf("minting with an empty key must fail, got %q", tok)
	}
	valid, _ := MintToken(testKey, "abc123", now, TokenTTL)
	if _, ok := VerifySigned(SessionKey(nil), valid, "abc123", now); ok {
		t.Fatal("verification with an empty key must be refused")
	}
}

// Each mint is distinct even for the same controller and expiry, so the burn
// list can retire an individual token.
func TestMintsAreDistinct(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, _ := MintToken(testKey, "abc123", now, TokenTTL)
	b, _ := MintToken(testKey, "abc123", now, TokenTTL)
	if a == b {
		t.Fatal("two mints produced an identical token — the nonce is not random")
	}
}

func TestBurnListRefusesAReplay(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var b burnList
	exp := now.Add(TokenTTL)
	if !b.use("nonce-1", exp, now) {
		t.Fatal("first use must succeed")
	}
	if b.use("nonce-1", exp, now) {
		t.Fatal("a replayed nonce must be refused")
	}
	// A different nonce is unaffected, and expired entries do not accumulate.
	if !b.use("nonce-2", exp, now) {
		t.Fatal("an unrelated nonce must succeed")
	}
	if got := b.len(now.Add(TokenTTL + time.Second)); got != 0 {
		t.Fatalf("burn list still holds %d expired entries", got)
	}
}

// The cookie is the longer-lived credential the browser replays on every
// request; it verifies through the same path as the bootstrap token.
func TestCookieVerifiesAndOutlivesTheToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c, err := MintCookie(testKey, "abc123", now, CookieTTL)
	if err != nil {
		t.Fatalf("mint cookie: %v", err)
	}
	if _, ok := VerifySigned(testKey, c, "abc123", now.Add(TokenTTL+time.Minute)); !ok {
		t.Fatal("the cookie must still verify after the bootstrap token's TTL")
	}
	if _, ok := VerifySigned(testKey, c, "abc123", now.Add(CookieTTL+time.Second)); ok {
		t.Fatal("the cookie must expire at its own TTL")
	}
}
