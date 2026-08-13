package idgen

import (
	"regexp"
	"testing"
)

func TestGUIDIsDNSSafeAndUnique(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}$`) // hex, valid single DNS label
	a, b := GUID(), GUID()
	if !re.MatchString(a) {
		t.Fatalf("GUID %q not 32-char hex", a)
	}
	if a == b {
		t.Fatal("two GUIDs collided")
	}
}

func TestEnrollmentCodeFormat(t *testing.T) {
	// Crockford alphabet 0-9 A-H J K M N P-T V-Z (excludes I, L, O, U).
	re := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)
	if c := EnrollmentCode(); !re.MatchString(c) {
		t.Fatalf("code %q wrong format", c)
	}
}
