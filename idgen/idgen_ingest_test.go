package idgen

import (
	"regexp"
	"testing"
)

var base62Re = regexp.MustCompile(`^[A-Za-z0-9]{30}$`)

func TestIngestIDFormat(t *testing.T) {
	id := IngestID()
	if len(id) != 30 {
		t.Fatalf("IngestID() len = %d, want 30", len(id))
	}
	if id[0] != 'u' {
		t.Fatalf("IngestID() = %q, want prefix 'u'", id)
	}
	if !base62Re.MatchString(id) {
		t.Fatalf("IngestID() = %q, contains chars outside [A-Za-z0-9]", id)
	}
}

func TestIngestSecretFormat(t *testing.T) {
	secret := IngestSecret()
	if len(secret) != 30 {
		t.Fatalf("IngestSecret() len = %d, want 30", len(secret))
	}
	if secret[0] != 'a' {
		t.Fatalf("IngestSecret() = %q, want prefix 'a'", secret)
	}
	if !base62Re.MatchString(secret) {
		t.Fatalf("IngestSecret() = %q, contains chars outside [A-Za-z0-9]", secret)
	}
}

func TestIngestSecretUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		s := IngestSecret()
		if seen[s] {
			t.Fatalf("duplicate IngestSecret() at iteration %d: %q", i, s)
		}
		seen[s] = true
	}
}
