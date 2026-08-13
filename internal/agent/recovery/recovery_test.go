package recovery

import (
	"regexp"
	"testing"
	"time"
)

const (
	testKey   = "-----BEGIN PRIVATE KEY-----\nnot a real key, only key material\n-----END PRIVATE KEY-----\n"
	testAgent = "0123456789abcdef0123456789abcdef"
)

// codeShape is EnrollmentCode's Crockford format — a recovery code is typed into
// the same field as an invite, so it must look the same.
var codeShape = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)

func TestCodeShape(t *testing.T) {
	code, err := Code(testKey, testAgent, 42)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	if !codeShape.MatchString(code) {
		t.Fatalf("code %q is not in the XXXX-XXXX Crockford shape", code)
	}
}

// TestCodeIsDeterministic is the property the whole design rests on: the
// read-only CLI and the verifying agent derive the SAME code without either
// writing state or talking to the other.
func TestCodeIsDeterministic(t *testing.T) {
	a, err := Code(testKey, testAgent, 7)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	b, err := Code(testKey, testAgent, 7)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	if a != b {
		t.Fatalf("same inputs gave %q and %q", a, b)
	}
}

// TestCodeVariesWithEveryInput pins the domain separation. A code that ignored
// the window would never expire; one that ignored the key would be the same on
// every relay in the fleet.
func TestCodeVariesWithEveryInput(t *testing.T) {
	base, err := Code(testKey, testAgent, 7)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	for _, tc := range []struct {
		name       string
		key, agent string
		window     int64
	}{
		{"a later window", testKey, testAgent, 8},
		{"a different relay key", testKey + "x", testAgent, 7},
		{"a different agent id", testKey, "ffffffffffffffffffffffffffffffff", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Code(tc.key, tc.agent, tc.window)
			if err != nil {
				t.Fatalf("Code: %v", err)
			}
			if got == base {
				t.Fatalf("%s produced the same code %q", tc.name, got)
			}
		})
	}
}

func TestCodeWithoutKeyMaterial(t *testing.T) {
	if _, err := Code("", testAgent, 1); err != ErrNoKeyMaterial {
		t.Fatalf("err = %v, want ErrNoKeyMaterial", err)
	}
}

// TestVerifyAcceptsTheCurrentAndPreviousWindow pins the skew tolerance — and
// pins that it stops at two, so the exposure stays bounded.
func TestVerifyAcceptsTheCurrentAndPreviousWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	current := WindowIndex(now)

	for _, tc := range []struct {
		name   string
		window int64
		want   bool
	}{
		{"the current window", current, true},
		{"the previous window", current - 1, true},
		{"two windows ago", current - 2, false},
		{"a future window", current + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := Code(testKey, testAgent, tc.window)
			if err != nil {
				t.Fatalf("Code: %v", err)
			}
			_, ok := Verify(testKey, testAgent, code, now, 0)
			if ok != tc.want {
				t.Fatalf("Verify(%s) = %v, want %v", tc.name, ok, tc.want)
			}
		})
	}
}

// TestVerifyHighWaterMarkIsSingleUse is the consumption rule. It covers the case
// a set of spent codes would miss: after consuming window N, the code for N-1 is
// still inside the skew tolerance and must be refused too.
func TestVerifyHighWaterMarkIsSingleUse(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	current := WindowIndex(now)

	used, ok := Verify(testKey, testAgent, mustCode(t, current), now, 0)
	if !ok || used != current {
		t.Fatalf("first use: ok=%v window=%d, want true/%d", ok, used, current)
	}
	// The agent stores `used` and passes used+1 from then on.
	if _, ok := Verify(testKey, testAgent, mustCode(t, current), now, used+1); ok {
		t.Fatal("SECURITY: the code was accepted a second time")
	}
	if _, ok := Verify(testKey, testAgent, mustCode(t, current-1), now, used+1); ok {
		t.Fatal("SECURITY: the still-in-skew PREVIOUS code was accepted after a redemption")
	}
	// The next window's code works, which is what "re-run the command" means.
	later := now.Add(Window)
	if _, ok := Verify(testKey, testAgent, mustCode(t, WindowIndex(later)), later, used+1); !ok {
		t.Fatal("a fresh code from the next window was refused")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	now := time.Now()
	for _, tc := range []string{"", "NOPE-NOPE", "0000-0000", mustCode(t, WindowIndex(now)) + "X"} {
		if _, ok := Verify(testKey, testAgent, tc, now, 0); ok {
			t.Fatalf("Verify accepted %q", tc)
		}
	}
}

// TestWindowEndIsInsideTheAcceptedRange guards the number the CLI prints: a user
// told "valid until X" must never find the code already refused before X.
func TestWindowEndIsInsideTheAcceptedRange(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	w := WindowIndex(now)
	justBeforeEnd := WindowEnd(w).Add(-time.Second)
	if _, ok := Verify(testKey, testAgent, mustCode(t, w), justBeforeEnd, 0); !ok {
		t.Fatal("the code was refused before the expiry the CLI advertises")
	}
}

func mustCode(t *testing.T, window int64) string {
	t.Helper()
	code, err := Code(testKey, testAgent, window)
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	return code
}
