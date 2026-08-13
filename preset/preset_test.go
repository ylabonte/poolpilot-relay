package preset

import "testing"

func TestSupportedOrder(t *testing.T) {
	got := Supported()
	want := []string{"procon-ip", "violet"}
	if len(got) != len(want) {
		t.Fatalf("Supported() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Supported() = %v, want %v", got, want)
		}
	}
}

func TestSupportedConstantsMatchWireValues(t *testing.T) {
	got := Supported()
	if got[0] != ProconIP {
		t.Errorf("Supported()[0] = %q, want ProconIP (%q)", got[0], ProconIP)
	}
	if got[1] != Violet {
		t.Errorf("Supported()[1] = %q, want Violet (%q)", got[1], Violet)
	}
}

func TestSupportedReturnsFreshSlice(t *testing.T) {
	first := Supported()
	first[0] = "mutated"

	second := Supported()
	if second[0] != ProconIP {
		t.Fatalf("Supported() shared backing array across calls: second call = %v", second)
	}
}

func TestIsSupported(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"procon-ip is supported", "procon-ip", true},
		{"violet is supported", "violet", true},
		{"empty string is not supported", "", false},
		{"unknown preset is not supported", "frog", false},
		{"comparison is case-sensitive (upper procon-ip)", "PROCON-IP", false},
		{"comparison is case-sensitive (upper Violet)", "Violet", false},
		{"whitespace is not trimmed", " procon-ip", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsSupported(c.in); got != c.want {
				t.Errorf("IsSupported(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
