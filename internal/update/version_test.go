package update

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.4.0", "v1.5.0", -1},
		{"v1.5.0", "v1.4.0", 1},
		{"v1.4.0", "v1.4.0", 0},
		{"1.4.0", "v1.4.0", 0},     // leading v is optional (§6.4)
		{"v1.4.0", "1.4.0", 0},     // ...on either side
		{"v1.10.0", "v1.9.0", 1},   // numeric, not lexical
		{"v2.0.0", "v1.99.99", 1},  // major dominates
		{"v1.4.1", "v1.4.0", 1},    // patch
		{"v01.04.00", "v1.4.0", 0}, // leading zeros canonicalize
	}
	for _, c := range cases {
		got, err := CompareVersions(c.a, c.b)
		if err != nil {
			t.Errorf("CompareVersions(%q,%q): unexpected error %v", c.a, c.b, err)
			continue
		}
		if got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareVersionsRejectsUnparseable(t *testing.T) {
	bad := []string{
		"dev",        // the ldflags default — must never win an ordering
		"",           // empty
		"v1.4",       // too few components
		"v1.4.0.1",   // too many
		"v1.4.x",     // non-numeric
		"v1.4.0-rc1", // pre-release
		"v1.4.0+b",   // build metadata
		"v+1.4.0",    // leading sign in a component
		"v-1.4.0",    // negative
		"v1..0",      // empty component
	}
	for _, s := range bad {
		if _, err := CompareVersions(s, "v1.0.0"); err == nil {
			t.Errorf("CompareVersions(%q, …): want error, got nil", s)
		}
		if _, err := CompareVersions("v1.0.0", s); err == nil {
			t.Errorf("CompareVersions(…, %q): want error, got nil", s)
		}
	}
}
